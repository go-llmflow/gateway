package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/go-llmflow/aws/eventstream"
	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/openai"
)

func goldenStreamBytes(u bedrockUsage) []byte {
	var b bytes.Buffer
	for _, ev := range bedrockEvents(u) {
		b.Write(chunkEvent(ev))
	}
	return b.Bytes()
}

func readAllSSE(t testing.TB, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b)
}

var bdrkPrices = pricesFor(config.Cost{Unit: "USD", InputPerMTok: 2, OutputPerMTok: 10, CachedPerMTok: 0.1, CacheWrite5mPerMTok: 2.5, CacheWrite1hPerMTok: 4})

// The byte-exact conversion: one SSE event per chunk, the event name from the
// payload's type, the payload as compact JSON.
func TestBedrockStreamGolden(t *testing.T) {
	c := newBedrockSSE(bytes.NewReader(goldenStreamBytes(bedrockUsage{10, 5, 100, 50})))
	got := readAllSSE(t, c)
	var want strings.Builder
	for _, ev := range bedrockEvents(bedrockUsage{10, 5, 100, 50}) {
		var h struct{ Type string }
		_ = json.Unmarshal([]byte(ev), &h)
		want.WriteString("event: " + h.Type + "\ndata: " + ev + "\n\n")
	}
	if got != want.String() {
		t.Errorf("got %q\nwant %q", got, want.String())
	}
	if !c.Complete() {
		t.Error("a whole stream is not complete")
	}
}

// Cut anywhere, a stream is not complete, and is charged W: no prefix of a
// valid stream, however long, under-bills.
func TestBedrockStreamEveryCut(t *testing.T) {
	full := goldenStreamBytes(bedrockUsage{10, 5, 100, 50})
	const w = int64(4_160_000) // nano-USD, 0.00416
	for n := 0; n < len(full); n++ {
		c := newBedrockSSE(bytes.NewReader(full[:n]))
		out := readAllSSE(t, c)
		if c.Complete() {
			t.Fatalf("a stream cut at byte %d of %d is complete", n, len(full))
		}
		st, _ := openai.CopySSEFor(openai.Messages, io.Discard, nil, strings.NewReader(out), false)
		if cost, atW := settleBedrockStream(c.Complete(), st, bdrkPrices, w); cost != w || !atW {
			t.Fatalf("cut at %d: charged %d, want W = %d", n, cost, w)
		}
		if !strings.Contains(out, "event: error") {
			t.Fatalf("cut at %d: the client is not told", n)
		}
	}
	// Corrupt any one byte: either a CRC/length error or an unreadable chunk;
	// never a complete stream that differs from the original.
	for i := 0; i < len(full); i++ {
		bad := bytes.Clone(full)
		bad[i] ^= 0x55
		c := newBedrockSSE(bytes.NewReader(bad))
		out := readAllSSE(t, c)
		if c.Complete() && out != readAllSSE(t, newBedrockSSE(bytes.NewReader(full))) {
			t.Fatalf("a flipped byte %d gives a complete, different stream", i)
		}
	}
}

// An exception event ends the stream, whatever followed it.
func TestBedrockStreamException(t *testing.T) {
	evs := bedrockEvents(bedrockUsage{10, 5, 100, 50})
	var b bytes.Buffer
	b.Write(chunkEvent(evs[0]))
	b.Write(exceptionEvent("modelStreamErrorException"))
	for _, e := range evs[1:] {
		b.Write(chunkEvent(e))
	}
	c := newBedrockSSE(&b)
	out := readAllSSE(t, c)
	if c.Complete() || strings.Contains(out, "content_block_delta") || !strings.Contains(out, "event: error") {
		t.Errorf("complete %v:\n%s", c.Complete(), out)
	}
	if strings.Contains(out, "Too many") {
		t.Errorf("the provider's exception text reached the client: %s", out)
	}
}

// Limits: a message announcing more than the bound, headers beyond the count
// are refused before they are read.
func TestBedrockStreamLimits(t *testing.T) {
	big := make([]byte, 0, 64)
	big = append(big, 0x7f, 0xff, 0xff, 0xff, 0, 0, 0, 0, 0, 0, 0, 0) // total_len 2 GiB, bad prelude CRC
	c := newBedrockSSE(bytes.NewReader(big))
	if out := readAllSSE(t, c); c.Complete() || !strings.Contains(out, "event: error") {
		t.Errorf("a 2 GiB message: %q", out)
	}
	many := eventstream.Message{Payload: []byte(`{}`)}
	for i := 0; i < 40; i++ {
		many.Headers = append(many.Headers, eventstream.Header{Name: fmt.Sprintf("h%d", i), Value: eventstream.String("x")})
	}
	raw, err := eventstream.Marshal(many)
	if err != nil {
		t.Fatal(err)
	}
	c = newBedrockSSE(bytes.NewReader(raw))
	if out := readAllSSE(t, c); c.Complete() || !strings.Contains(out, "event: error") {
		t.Errorf("40 headers: %q", out)
	}
}

// FuzzBedrockStream feeds random frames through the stream path: it must
// never panic, always end, always emit well-formed SSE, and never charge less
// than the usage a complete stream delivered nor less than W for an
// incomplete one.
func FuzzBedrockStream(f *testing.F) {
	f.Add([]byte{}, uint8(0), uint32(10), uint32(5), uint32(100), uint32(50))
	f.Add(goldenStreamBytes(bedrockUsage{10, 5, 100, 50}), uint8(0), uint32(10), uint32(5), uint32(100), uint32(50))
	f.Add([]byte("garbage"), uint8(1), uint32(1), uint32(1), uint32(1), uint32(1))
	f.Add([]byte{0x00, 0x00, 0x00, 0x10}, uint8(2), uint32(0), uint32(0), uint32(0), uint32(0))
	f.Add([]byte(`{"type":"message_stop"}`), uint8(3), uint32(7), uint32(7), uint32(7), uint32(7))
	f.Add([]byte(`{"type":"message_delta","usage":{"output_tokens":999999999}}`), uint8(3), uint32(7), uint32(7), uint32(7), uint32(7))
	const w = int64(1) << 60
	f.Fuzz(func(t *testing.T, data []byte, mode uint8, in, out, cr, cw uint32) {
		u := bedrockUsage{In: int64(in), Out: int64(out), Read: int64(cr), Write: int64(cw)}
		var src []byte
		switch mode % 5 {
		case 0: // arbitrary bytes
			src = data
		case 1: // a valid stream, then garbage
			src = append(goldenStreamBytes(u), data...)
		case 2: // a valid stream with one byte changed
			src = goldenStreamBytes(u)
			if len(data) > 0 {
				src[int(data[0])*7%len(src)] ^= 0x80 | data[len(data)-1]
			}
		case 3: // the data as the decoded content of a chunk, between a start and a stop
			src = append(append(chunkEvent(bedrockEvents(u)[0]), chunkEvent(string(data))...), chunkEvent(`{"type":"message_stop"}`)...)
		case 4: // a valid stream
			src = goldenStreamBytes(u)
		}
		c := newBedrockSSE(bytes.NewReader(src))
		sse, err := io.ReadAll(c)
		if err != nil {
			t.Fatalf("the converter fails with %v instead of ending", err)
		}
		// Well-formed: blocks of "event: <type>\ndata: <json>".
		var types []string
		for _, blk := range strings.Split(strings.TrimSuffix(string(sse), "\n\n"), "\n\n") {
			if blk == "" {
				continue
			}
			ev, js, ok := strings.Cut(blk, "\ndata: ")
			if !ok || !strings.HasPrefix(ev, "event: ") || !json.Valid([]byte(js)) || strings.ContainsAny(js, "\n\r") {
				t.Fatalf("malformed SSE block %q", blk)
			}
			types = append(types, strings.TrimPrefix(ev, "event: "))
		}
		st, _ := openai.CopySSEFor(openai.Messages, io.Discard, nil, bytes.NewReader(sse), false)
		cost, atW := settleBedrockStream(c.Complete(), st, bdrkPrices, w)
		if !c.Complete() {
			if cost != w || !atW {
				t.Fatalf("an incomplete stream was charged %d, want W", cost)
			}
			return
		}
		// Complete: a start, a usage delta and a stop, in this order, and the
		// charge is that of the usage delivered.
		si, di, pi := -1, -1, -1
		for i, ty := range types {
			switch {
			case ty == "message_start" && si < 0:
				si = i
			case ty == "message_delta" && di < 0:
				di = i
			case ty == "message_stop" && pi < 0:
				pi = i
			}
		}
		if !(0 <= si && si < di && di < pi) {
			t.Fatalf("complete without start < delta < stop: %v", types)
		}
		if !st.HaveUsage {
			t.Fatal("complete, but the usage was not read")
		}
		if want := bdrkPrices.costNano(st.Usage); cost != want {
			t.Fatalf("charged %d, usage costs %d", cost, want)
		}
		if mode%5 == 4 { // the independent oracle
			prompt := u.In + u.Read + u.Write
			// message_start reports output_tokens: 1; the delta's count is
			// cumulative, the larger of the two is the output.
			u.Out = max(u.Out, 1)
			oracle := bdrkPrices.costNano(openai.Usage{PromptTokens: prompt, CompletionTokens: u.Out, TotalTokens: prompt + u.Out,
				PromptTokensDetails: &struct {
					CachedTokens int64 `json:"cached_tokens"`
				}{u.Read}, CacheWrite5m: u.Write})
			if cost != oracle {
				t.Fatalf("complete stream of %+v charged %d, oracle %d", u, cost, oracle)
			}
		}
	})
}

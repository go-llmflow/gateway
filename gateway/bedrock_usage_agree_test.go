package gateway

import (
	"bytes"
	"io"
	"testing"

	"github.com/go-llmflow/gateway/openai"
)

// Issue #157: FuzzBedrockStream found a stream the converter calls complete
// while CopySSEFor, reading the converter's own output, found no usage in it.
// The two must agree on what a final usage is, whatever the bytes of the
// message_delta: here, a usage that is valid field by field but overflows when
// summed, one whose key is spelled with an escape, and counts out of range.
func TestTheConverterAndTheUsageTrackerAgreeOnTheFinalUsage(t *testing.T) {
	u := bedrockUsage{In: 10, Out: 5, Read: 100, Write: 50}
	for name, delta := range map[string]string{
		"sum overflows":          `{"type":"message_delta","usage":{"output_tokens":1,"input_tokens":9223372036854775807,"cache_read_input_tokens":1}}`,
		"three counts overflow":  `{"type":"message_delta","usage":{"output_tokens":1,"input_tokens":4611686018427387904,"cache_read_input_tokens":4611686018427387904,"cache_creation_input_tokens":4611686018427387904}}`,
		"escaped key":            `{"type":"message_delta","usage":{"output_tokens":7}}`,
		"escaped nested key":     `{"type":"message_delta","usage":{"output_tokens":7}}`,
		"output tokens overflow": `{"type":"message_delta","usage":{"output_tokens":9223372036854775807}}`,
		"sane":                   `{"type":"message_delta","usage":{"output_tokens":7}}`,
	} {
		t.Run(name, func(t *testing.T) {
			src := append(append(chunkEvent(bedrockEvents(u)[0]), chunkEvent(delta)...), chunkEvent(`{"type":"message_stop"}`)...)
			c := newBedrockSSE(bytes.NewReader(src))
			sse, err := io.ReadAll(c)
			if err != nil {
				t.Fatal(err)
			}
			st, _ := openai.CopySSEFor(openai.Messages, io.Discard, nil, bytes.NewReader(sse), false)
			if c.Complete() && !st.HaveUsage {
				t.Errorf("the stream is complete, but the usage in %s was not read", delta)
			}
			cost, atW := settleBedrockStream(c.Complete(), st, bdrkPrices, int64(1)<<60)
			if c.Complete() && st.HaveUsage && (atW || cost != bdrkPrices.costNano(st.Usage)) {
				t.Errorf("complete with usage, charged %d (atW %v)", cost, atW)
			}
		})
	}
}

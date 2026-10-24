package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/go-llmflow/gateway/audit"
	"github.com/go-llmflow/gateway/config"
	"github.com/spf13/cobra"
)

func auditCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "audit", Short: "Verify and manage the audit log"}
	cmd.AddCommand(auditVerifyCmd(), auditKeygenCmd(), auditPubkeyCmd())
	return cmd
}

// verifyLog checks the log and writes a report; it fails when the log does
// not verify.
func verifyLog(ctx context.Context, st audit.Store, keys []ed25519.PublicKey, w io.Writer) error {
	r, err := audit.Verify(ctx, st, keys)
	if err != nil {
		return err
	}
	if r.Entries == 0 {
		fmt.Fprintf(w, "no entries; %d checkpoints\n", r.Checkpoints)
	} else {
		fmt.Fprintf(w, "entries %d to %d (%d), %d checkpoints verified, %d entries after the newest checkpoint\n",
			r.First, r.Last, r.Entries, r.Checkpoints, r.Unsigned)
	}
	for _, p := range r.Problems {
		fmt.Fprintln(w, "PROBLEM:", p)
	}
	if !r.OK() {
		return fmt.Errorf("the audit log does not verify: %d problems", len(r.Problems))
	}
	fmt.Fprintln(w, "ok")
	return nil
}

func auditVerifyCmd() *cobra.Command {
	var path string
	var pubs []string
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Recompute the audit chain and check its signed checkpoints",
		Long: "Recompute every entry's hash, check that each chains from the one before, and check every checkpoint's\n" +
			"signature. Give the public keys with --public-key (best: from somewhere the gateway's operator cannot change);\n" +
			"without one, the key is derived from the configuration's signing_key_file.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := config.Load(path)
			if err != nil {
				return err
			}
			var keys []ed25519.PublicKey
			for _, p := range pubs {
				k, err := audit.ParsePublicKey(p)
				if err != nil {
					return err
				}
				keys = append(keys, k)
			}
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if len(keys) == 0 && c.Audit.SigningKeyFile != "" {
				res, err := resolver(c)
				if err != nil {
					return err
				}
				seed, err := res.Resolve(ctx, c.Audit.SigningKeyFile)
				if err != nil {
					return fmt.Errorf("audit signing_key_file: %w", err)
				}
				k, err := audit.ParsePrivateKey(seed)
				if err != nil {
					return err
				}
				keys = append(keys, k.Public().(ed25519.PublicKey))
			}
			if c.Store.Kind != "postgres" {
				return errors.New("audit verify reads the PostgreSQL store; the memory store keeps nothing after its process")
			}
			st, err := openStore(ctx, c)
			if err != nil {
				return err
			}
			defer st.Close()
			return verifyLog(ctx, st, keys, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVarP(&path, "config", "c", "", "configuration file (HCL)")
	cmd.Flags().StringArrayVar(&pubs, "public-key", nil, "Ed25519 public key (base64 or hex) that may sign checkpoints; repeatable")
	_ = cmd.MarkFlagRequired("config")
	return cmd
}

func auditKeygenCmd() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "keygen",
		Short: "Create a checkpoint signing key (an Ed25519 seed) in a new file, and print its public key",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pub, priv, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				return err
			}
			// O_EXCL: never overwrite a key, which would orphan every
			// checkpoint it signed.
			f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintln(f, base64.StdEncoding.EncodeToString(priv.Seed())); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "public key %s (id %s)\n", base64.StdEncoding.EncodeToString(pub), audit.KeyID(pub))
			return nil
		},
	}
	cmd.Flags().StringVarP(&out, "out", "o", "", "file to create (mode 0600)")
	_ = cmd.MarkFlagRequired("out")
	return cmd
}

func auditPubkeyCmd() *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "pubkey",
		Short: "Print the public key of the configured checkpoint signing key",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := config.Load(path)
			if err != nil {
				return err
			}
			if c.Audit.SigningKeyFile == "" {
				return errors.New("no audit signing_key_file in the configuration")
			}
			res, err := resolver(c)
			if err != nil {
				return err
			}
			seed, err := res.Resolve(context.Background(), c.Audit.SigningKeyFile)
			if err != nil {
				return err
			}
			k, err := audit.ParsePrivateKey(seed)
			if err != nil {
				return err
			}
			pub := k.Public().(ed25519.PublicKey)
			fmt.Fprintf(cmd.OutOrStdout(), "public key %s (id %s)\n", base64.StdEncoding.EncodeToString(pub), audit.KeyID(pub))
			return nil
		},
	}
	cmd.Flags().StringVarP(&path, "config", "c", "", "configuration file (HCL)")
	_ = cmd.MarkFlagRequired("config")
	return cmd
}

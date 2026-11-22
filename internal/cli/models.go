/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/TxnLab/zs-proxy/internal/connect"
)

// newModelsCmd is `zs-proxy models`: the running proxy's model catalog, with
// the operators' tags, optionally narrowed to the models carrying given tags.
func newModelsCmd() *cobra.Command {
	var (
		f       proxyFlags
		tags    []string
		asJSON  bool
		urlFlag string
	)
	cmd := &cobra.Command{
		Use:   "models",
		Short: "List the models the running proxy can route to, with their tags",
		Long: "List the models the running proxy advertises: context window, lowest payer-net price,\n" +
			"capabilities and tags (assigned by operators or taken from the model's Hugging Face\n" +
			"card). --tag keeps only the models carrying that tag (case-insensitive); repeat it to\n" +
			"require several. A tag is a node's own claim, and one node tagging a model is enough\n" +
			"to list it: `curl <proxy>/v1/zs/operators` shows which node declared which tags.",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			for i, t := range tags {
				tags[i] = strings.TrimSpace(t)
				if tags[i] == "" {
					return fmt.Errorf("--tag needs a non-empty value")
				}
			}
			ep, err := resolveProxyEndpoint(cmd, &f, urlFlag)
			if err != nil {
				return err
			}
			all, err := connect.FetchModels(cmd.Context(), ep.BaseURL)
			if err != nil {
				return fmt.Errorf("list models: %w", err)
			}
			models := connect.WithTags(all, tags)
			connect.SortModels(models)
			if asJSON {
				return writeModelsJSON(cmd.OutOrStdout(), models)
			}
			if len(models) == 0 {
				switch {
				case len(tags) > 0 && !anyTagged(all):
					// A proxy older than tag support, or one answering only the
					// id-only /v1/models fallback, reads exactly like a catalog with
					// no match — so say which it might be rather than "none".
					fmt.Fprintf(cmd.ErrOrStderr(), "No model in the catalog carries any tags, so none match %s.\n"+
						"If you expected some, the running proxy may predate tag support: restart it with `zs-proxy proxy restart`.\n",
						strings.Join(tags, " + "))
				case len(tags) > 0:
					fmt.Fprintf(cmd.ErrOrStderr(), "No models tagged %s.\n", strings.Join(tags, " + "))
				default:
					fmt.Fprintln(cmd.ErrOrStderr(), "The proxy advertises no models yet.")
				}
				return nil
			}
			writeModelsTable(cmd.OutOrStdout(), models)
			return nil
		},
	}
	f.bindCommon(cmd)
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "only models carrying this tag (repeatable; all must match)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the catalog as JSON")
	cmd.Flags().StringVar(&urlFlag, "url", "", "advanced: read this OpenAI base URL instead of the local proxy (skips the running check)")
	return cmd
}

func anyTagged(models []connect.Model) bool {
	for _, m := range models {
		if len(m.Tags) > 0 {
			return true
		}
	}
	return false
}

// modelJSON is the --json shape. Rates are the lowest payer-net offer, as in the
// table, and an absent rate means no operator declared one (connect.Model's 0);
// tags are always an array so `jq 'index("code")'` never trips over a null.
type modelJSON struct {
	ID                string   `json:"id"`
	ContextWindow     int      `json:"context_window,omitempty"`
	MaxOutputTokens   int      `json:"max_output_tokens,omitempty"`
	InputUSDPer1M     float64  `json:"input_usd_per_1m,omitempty"`
	OutputUSDPer1M    float64  `json:"output_usd_per_1m,omitempty"`
	CacheReadUSDPer1M *float64 `json:"cache_read_usd_per_1m,omitempty"`
	InputModalities   []string `json:"input_modalities,omitempty"`
	OutputModalities  []string `json:"output_modalities,omitempty"`
	ToolUse           *bool    `json:"tool_use,omitempty"`
	Reasoning         bool     `json:"reasoning"`
	Tags              []string `json:"tags"`
}

func writeModelsJSON(w io.Writer, models []connect.Model) error {
	out := make([]modelJSON, 0, len(models))
	for _, m := range models {
		tags := m.Tags
		if tags == nil {
			tags = []string{}
		}
		out = append(out, modelJSON{
			ID:                m.ID,
			ContextWindow:     m.ContextWindow,
			MaxOutputTokens:   m.MaxOutputTokens,
			InputUSDPer1M:     m.InputUSDPer1M,
			OutputUSDPer1M:    m.OutputUSDPer1M,
			CacheReadUSDPer1M: m.CacheReadUSDPer1M,
			InputModalities:   m.InputModalities,
			OutputModalities:  m.OutputModalities,
			ToolUse:           m.ToolUse,
			Reasoning:         m.Reasoning,
			Tags:              tags,
		})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func writeModelsTable(w io.Writer, models []connect.Model) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tCONTEXT\tIN $/1M\tOUT $/1M\tFEATURES\tTAGS")
	for _, m := range models {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			printable(m.ID), contextLabel(m.ContextWindow), rateLabel(m.InputUSDPer1M), rateLabel(m.OutputUSDPer1M),
			orDash(strings.Join(modelFeatures(m), ",")), orDash(printable(strings.Join(m.Tags, ","))))
	}
	_ = tw.Flush()
}

// printable replaces every non-printable rune with '?'. Ids and tags are
// node-authored and land on a terminal here, so an ESC sequence would reach the
// user's tty and a tab or newline would break the table. The JSON path needs no
// such pass: the encoder escapes control characters.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return '?'
	}, s)
}

func contextLabel(tokens int) string {
	if tokens <= 0 {
		return "-"
	}
	return fmt.Sprintf("%dK", tokens/1024)
}

func rateLabel(usdPer1M float64) string {
	if usdPer1M <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.2f", usdPer1M)
}

// modelFeatures names the capabilities worth scanning a column for. Tools is
// listed only on an explicit true: unadvertised is not a claim either way.
func modelFeatures(m connect.Model) []string {
	var out []string
	if m.ToolUse != nil && *m.ToolUse {
		out = append(out, "tools")
	}
	if m.Reasoning {
		out = append(out, "reasoning")
	}
	for _, in := range m.InputModalities {
		if in == "image" {
			out = append(out, "vision")
			break
		}
	}
	for _, o := range m.OutputModalities {
		if o == "image" {
			out = append(out, "image-out")
			break
		}
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

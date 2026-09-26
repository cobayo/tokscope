package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"unicode/utf8"
)

func cmdTail(args []string) int {
	f := flag.NewFlagSet("tail", flag.ContinueOnError)
	n := f.Int("n", 10, "表示する件数")
	asJSON := f.Bool("json", false, "JSON Lines で出力します（プロンプト全文を含みます）")
	if err := f.Parse(args); err != nil {
		return 2
	}
	recs, err := readLastRecords(logPath(homeDir()), *n)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintln(os.Stderr, "tokscoop:", err)
		return 1
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		for _, r := range recs {
			_ = enc.Encode(r)
		}
		return 0
	}
	if len(recs) == 0 {
		fmt.Println("まだ記録がありません。tokscope run -- claude のように起動してください。")
		return 0
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "TIME\tCLIENT\tMODEL\t%11s\t%5s\t%9s\tPROMPT\n", "INPUT", "CACHE", "OUTPUT")
	for i := len(recs) - 1; i >= 0; i-- {
		r := recs[i]
		in, cache, out := "-", "-", "-"
		if r.UsageFound {
			in, out = comma(r.TotalInputTokens), comma(r.OutputTokens)
			if r.TotalInputTokens > 0 {
				cache = strconv.FormatInt(r.CacheReadTokens*100/r.TotalInputTokens, 10) + "%"
			}
		}
		if r.Status >= 300 || r.Status == 0 {
			out = fmt.Sprintf("HTTP %d", r.Status)
		}
		prompt := firstLine(r.Prompt, 50)
		if r.PromptKind == "tool_result" {
			prompt = "↳ " + prompt
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%11s\t%5s\t%9s\t%s\n",
			r.Time.Local().Format("15:04:05"), r.Client, firstLine(r.Model, 32), in, cache, out, prompt)
	}
	_ = tw.Flush()
	return 0
}

func readLastRecords(path string, n int) ([]Record, error) {
	if n <= 0 {
		n = 10
	}
	var ring []Record
	err := forEachRecord(path, func(r Record) {
		ring = append(ring, r)
		if len(ring) > n {
			ring = ring[1:]
		}
	})
	return ring, err
}

func comma(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func firstLine(s string, max int) string {
	s, _, cut := strings.Cut(strings.TrimSpace(s), "\n")
	if utf8.RuneCountInString(s) > max {
		return string([]rune(s)[:max]) + "…"
	}
	if cut {
		return s + " …"
	}
	return s
}

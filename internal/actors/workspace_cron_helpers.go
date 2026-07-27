package actors

import (
	"strconv"
	"strings"
	"time"
)

// cron command parsing/formatting helpers (kept separate from the command
// logic for readability). Names are cron-suffixed where a generic name would
// collide with existing workspace helpers.

// nextToken splits off the first whitespace-delimited token, returning it and
// the trimmed remainder.
func nextToken(s string) (token, rest string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i], strings.TrimSpace(s[i+1:])
	}
	return s, ""
}

// nextQuotedToken splits off the first token, honouring a leading double or
// single quote so a multi-word cron schedule ("0 9 * * *") is one token.
// Falls back to whitespace splitting for an unquoted token (e.g. "@daily").
func nextQuotedToken(s string) (token, rest string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	if q := s[0]; q == '"' || q == '\'' {
		if end := strings.IndexByte(s[1:], q); end >= 0 {
			return s[1 : 1+end], strings.TrimSpace(s[1+end+1:])
		}
		// Unterminated quote: take the rest as the token.
		return strings.TrimSpace(s[1:]), ""
	}
	return nextToken(s)
}

func parseIntCron(s string) (int, error) { return strconv.Atoi(strings.TrimSpace(s)) }

// extractSchedule pulls a cron schedule off the front of s, returning it and
// the remainder. It handles BOTH forms:
//
//   - quoted: `"0 9 * * *" rest` or `'@every 15m' rest` (typed in a session,
//     where quotes survive in the raw command text)
//   - unquoted: `0 9 * * * rest`, `@every 15m rest`, `@daily rest` (the CLI
//     `--cron` flag path space-joins args, dropping the quotes)
//
// Unquoted disambiguation is deterministic: `@every` takes 2 tokens, any other
// `@descriptor` takes 1, and a standard cron takes exactly its 5 fields.
func extractSchedule(s string) (schedule, rest string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	if s[0] == '"' || s[0] == '\'' {
		return nextQuotedToken(s)
	}
	fields := strings.Fields(s)
	take := 5 // standard 5-field cron
	if strings.HasPrefix(fields[0], "@") {
		if strings.EqualFold(fields[0], "@every") {
			take = 2
		} else {
			take = 1
		}
	}
	if len(fields) < take {
		take = len(fields)
	}
	schedule = strings.Join(fields[:take], " ")
	rest = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), schedule))
	return schedule, rest
}

// fmtCronTime renders a run/next time compactly, or "—" for the zero time.
func fmtCronTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Format("Mon 15:04 Jan-02")
}

// tzSuffix renders " (TZ)" when a timezone is set.
func tzSuffix(tz string) string {
	if strings.TrimSpace(tz) == "" {
		return ""
	}
	return " (" + tz + ")"
}

// orDash returns s, or "—" when empty.
func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// truncateCron shortens s to maxLen with an ellipsis.
func truncateCron(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 1 {
		return s[:maxLen]
	}
	return s[:maxLen-1] + "…"
}

// firstLineCron returns the first non-empty line of s.
func firstLineCron(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

package query

import "strings"

// FTSMatch turns search-bar input into an FTS5 MATCH expression. Bare terms
// become quoted prefix queries (`order` -> `"order"*`) so they match longer
// tokens and punctuation can't break MATCH syntax; combined with the porter
// tokenizer this makes "order" find orders/ordering. Input using FTS5
// operators or syntax characters is passed through untouched for power users.
func FTSMatch(input string) string {
	input = strings.TrimSpace(input)
	if input == "" {
		return input
	}
	terms := strings.Fields(input)
	for _, t := range terms {
		switch t {
		case "AND", "OR", "NOT", "NEAR":
			return input
		}
		// note: ':' is NOT here on purpose - a colon in bare input is log
		// text (u_991:), and quoting neutralizes its column-filter meaning
		if strings.ContainsAny(t, `"*()^`) {
			return input
		}
	}
	out := make([]string, len(terms))
	for i, t := range terms {
		out[i] = `"` + t + `"*`
	}
	return strings.Join(out, " ")
}

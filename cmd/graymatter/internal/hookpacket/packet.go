// Package hookpacket selects whole facts from a caller-supplied recall snapshot.
// It neither retrieves memories nor changes their text or access metadata.
package hookpacket

import (
	"context"
	"errors"
	"math"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const (
	// MaxCandidates bounds scoring work, including graph-enriched recall results.
	MaxCandidates = 32
	// Input limits bound synchronous normalization and scoring work. They are
	// independent of the smaller output budget chosen by the hook.
	MaxTextBytes      = 64 << 10
	MaxCandidateBytes = 256 << 10
)

// Result holds the payload and text-free selection metadata. Selected contains
// positions in the supplied snapshot, not persistent fact IDs. OmissionReason
// describes an empty payload; it does not assess whether the query is answered.
type Result struct {
	Facts          []string
	Selected       []int
	Candidates     int
	Bytes          int
	OmissionReason string
}

// Select ranks facts by cosine TF-IDF similarity divided by the square root of
// their UTF-8 byte length. Document frequencies use the entire snapshot. Equal
// utilities retain recall order. Packing skips facts that do not fit and counts
// the two newline bytes between whole facts; it never truncates a fact.
//
// maxFacts must be 1..8 and maxBytes positive. Invalid, duplicate, oversized or
// cancelled inputs return an error with no partial payload. An empty query is
// valid: all utilities are zero, so packing follows recall order.
func Select(ctx context.Context, query string, facts []string, maxFacts, maxBytes int) (Result, error) {
	if ctx == nil {
		return Result{}, errors.New("selection context is required")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if maxFacts < 1 || maxFacts > 8 || maxBytes <= 0 {
		return Result{}, errors.New("selection requires 1..8 facts and a positive byte budget")
	}
	if len(facts) > MaxCandidates {
		return Result{}, errors.New("candidate limit exceeded")
	}
	if len(query) > MaxTextBytes || !utf8.ValidString(query) {
		return Result{}, errors.New("invalid or oversized selection query")
	}
	seen := make(map[string]struct{}, len(facts))
	total := 0
	for _, fact := range facts {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if len(fact) > MaxTextBytes || !utf8.ValidString(fact) || strings.TrimSpace(fact) == "" {
			return Result{}, errors.New("invalid or oversized candidate")
		}
		total += len(fact)
		if total > MaxCandidateBytes {
			return Result{}, errors.New("candidate byte limit exceeded")
		}
		if _, exists := seen[fact]; exists {
			return Result{}, errors.New("duplicate candidate")
		}
		seen[fact] = struct{}{}
	}
	result := Result{Facts: []string{}, Selected: []int{}, Candidates: len(facts)}
	if len(facts) == 0 {
		result.OmissionReason = "no_candidates"
		return result, nil
	}
	scores, err := lexicalScores(ctx, query, facts)
	if err != nil {
		return Result{}, err
	}
	order := make([]int, len(facts))
	for i := range facts {
		order[i] = i
		scores[i] /= math.Sqrt(float64(len(facts[i])))
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if scores[a] == scores[b] {
			return a < b
		}
		return scores[a] > scores[b]
	})
	for _, i := range order {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if len(result.Facts) == maxFacts {
			break
		}
		size := len(facts[i])
		if len(result.Facts) > 0 {
			size += 2
		}
		if size > maxBytes-result.Bytes {
			continue
		}
		result.Facts = append(result.Facts, facts[i])
		result.Selected = append(result.Selected, i)
		result.Bytes += size
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if len(result.Facts) == 0 {
		result.OmissionReason = "no_fact_fits_budget"
	}
	return result, nil
}

// Terms use NFKC followed by full Unicode case folding and alphanumeric runs.
// Negation and accented words are retained; Recall's ASCII stemmer is not used.
func frequencies(ctx context.Context, text string) (map[string]float64, []string, error) {
	text = cases.Fold().String(norm.NFKC.String(text))
	terms := strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	freq := make(map[string]float64)
	order := make([]string, 0)
	for i, word := range terms {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
		}
		if freq[word] == 0 {
			order = append(order, word)
		}
		freq[word]++
	}
	return freq, order, ctx.Err()
}

func lexicalScores(ctx context.Context, query string, facts []string) ([]float64, error) {
	q, queryOrder, err := frequencies(ctx, query)
	if err != nil {
		return nil, err
	}
	docs := make([]map[string]float64, len(facts))
	orders := make([][]string, len(facts))
	df := make(map[string]int)
	for i, fact := range facts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		docs[i], orders[i], err = frequencies(ctx, fact)
		if err != nil {
			return nil, err
		}
		for _, word := range orders[i] {
			df[word]++
		}
	}
	idf := func(word string) float64 {
		return 1 + math.Log(float64(1+len(facts))/float64(1+df[word]))
	}
	queryNorm := 0.0
	for _, word := range queryOrder {
		v := q[word] * idf(word)
		queryNorm += v * v
	}
	queryNorm = math.Sqrt(queryNorm)
	out := make([]float64, len(facts))
	for i, doc := range docs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		norm, dot := 0.0, 0.0
		for _, word := range orders[i] {
			v := doc[word] * idf(word)
			norm += v * v
		}
		norm = math.Sqrt(norm)
		for _, word := range queryOrder {
			dot += (q[word] * idf(word)) * (doc[word] * idf(word))
		}
		if queryNorm > 0 && norm > 0 {
			out[i] = dot / (queryNorm * norm)
		}
	}
	return out, ctx.Err()
}

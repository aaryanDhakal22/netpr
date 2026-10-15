// Package pagerange parses page selections like "1-3, 5, 8-10".
package pagerange

import (
	"fmt"
	"strconv"
	"strings"
)

// Parse validates spec against a document of total pages. It returns the
// selection in canonical form (sorted, merged, e.g. "1-3,5") and the number
// of selected pages. An empty spec selects every page and returns "".
func Parse(spec string, total int) (string, int, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "", total, nil
	}
	selected := make([]bool, total+1)
	for item := range strings.SplitSeq(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(item, "-")
		from, err := page(lo, total)
		if err != nil {
			return "", 0, err
		}
		to := from
		if isRange {
			if strings.TrimSpace(hi) == "" {
				to = total // "5-" means through the end
			} else if to, err = page(hi, total); err != nil {
				return "", 0, err
			}
		}
		if to < from {
			return "", 0, fmt.Errorf("range %q runs backwards", item)
		}
		for p := from; p <= to; p++ {
			selected[p] = true
		}
	}

	var parts []string
	count := 0
	for p := 1; p <= total; p++ {
		if !selected[p] {
			continue
		}
		start := p
		for p+1 <= total && selected[p+1] {
			p++
		}
		count += p - start + 1
		if start == p {
			parts = append(parts, strconv.Itoa(start))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", start, p))
		}
	}
	if count == 0 {
		return "", 0, fmt.Errorf("page range %q selects no pages", spec)
	}
	canon := strings.Join(parts, ",")
	if count == total {
		canon = "" // whole document; no split needed
	}
	return canon, count, nil
}

func page(s string, total int) (int, error) {
	s = strings.TrimSpace(s)
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a page number", s)
	}
	if n < 1 || n > total {
		return 0, fmt.Errorf("page %d is outside 1-%d", n, total)
	}
	return n, nil
}

// Sheets estimates physical sheets for pages printed copies times.
func Sheets(pages, copies int, duplex bool) int {
	per := pages
	if duplex {
		per = (pages + 1) / 2
	}
	return per * copies
}

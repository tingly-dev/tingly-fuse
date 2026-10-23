package session

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Labels are free-form key/value tags on a session, set by whoever manages
// sessions (a script, an agent framework) and never interpreted here. They
// let such a caller find its sessions again without keeping its own index.
const (
	maxLabelKey   = 63
	maxLabelValue = 256
)

var validLabelKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// ValidateLabels checks every key and value.
func ValidateLabels(labels map[string]string) error {
	for k, v := range labels {
		if err := validateLabel(k, v); err != nil {
			return err
		}
	}
	return nil
}

func validateLabel(k, v string) error {
	if len(k) > maxLabelKey || !validLabelKey.MatchString(k) {
		return fmt.Errorf("invalid label key %q: up to %d letters, digits, '.', '_', '-' or '/', starting with a letter or digit", k, maxLabelKey)
	}
	if len(v) > maxLabelValue || strings.ContainsFunc(v, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		return fmt.Errorf("invalid value for label %q: up to %d bytes, no control characters", k, maxLabelValue)
	}
	return nil
}

// ParseLabels turns "key=value" arguments into a label set.
func ParseLabels(args []string) (map[string]string, error) {
	out := map[string]string{}
	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		if !ok {
			return nil, fmt.Errorf("label %q: want key=value", a)
		}
		if err := validateLabel(k, v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

// Selector picks sessions by their labels; every term must match.
type Selector []selectorTerm

type selectorTerm struct {
	key, value string
	hasValue   bool
}

// ParseSelector reads terms of the form "key=value" (the label has that
// value) and "key" (the label is set, to anything).
func ParseSelector(terms []string) (Selector, error) {
	var sel Selector
	for _, t := range terms {
		k, v, hasValue := strings.Cut(t, "=")
		if !validLabelKey.MatchString(k) || len(k) > maxLabelKey {
			return nil, fmt.Errorf("invalid label selector %q", t)
		}
		sel = append(sel, selectorTerm{k, v, hasValue})
	}
	return sel, nil
}

// Match reports whether labels satisfy every term. An empty selector
// matches everything.
func (s Selector) Match(labels map[string]string) bool {
	for _, t := range s {
		v, ok := labels[t.key]
		if !ok || (t.hasValue && v != t.value) {
			return false
		}
	}
	return true
}

// FormatLabels renders labels as sorted "key=value" pairs, or "-" for none.
func FormatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		keys[i] = k + "=" + labels[k]
	}
	return strings.Join(keys, ",")
}

package main

import (
	"errors"
	"fmt"
	"testing"
)

func TestAFatalErrorCarriesItsKind(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want errKind
	}{
		{"classified", kindErrf(errMissingKey, "no key"), errMissingKey},
		{"unclassified", errf("something broke"), errOther},
		{"wrapped classified", fmt.Errorf("stage: %w", kindErrf(errNoModel, "no model")), errNoModel},
		{"a plain error", errors.New("plain"), errOther},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := kindOf(tc.err); got != tc.want {
				t.Errorf("kindOf = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAFatalErrorKeepsItsMessageExactly(t *testing.T) {
	err := kindErrf(errRateLimited, "translate: %v", errors.New("429"))
	if got, want := err.Error(), "translate: 429"; got != want {
		t.Errorf("Error() = %q, want %q — the message is part of the contract", got, want)
	}
}

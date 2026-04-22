package ui

// v0.5.8 coverage for the Members-page Select-button fix (#118).
//
// The bug: the Select button in SearchResultsFragment used an inline
// onclick with the name and Redpoint ID embedded as JS string literals
// between single quotes. HTMLEscape (template.HTMLEscapeString) turns
// apostrophes and double quotes into numeric entity references
// (`&#39;`, `&#34;`). The HTML attribute parser decodes those back to
// literal `'` and `"` inside the onclick value, so the JS parser
// eventually saw a string literal terminated mid-name — e.g.
// `value='O'Brien'` — and raised a SyntaxError that left the button
// silently doing nothing. Names with any such character broke.
//
// The fix moves the values to data-* attributes and puts the click
// logic in a delegated handler in members.html. Data attributes are
// safe under HTMLEscape because getAttribute returns the raw decoded
// string — the JS never has to quote it.
//
// These tests pin the new rendering:
//   - no inline onclick on the Select button
//   - the button carries the data-* attributes used by the click
//     handler
//   - names and IDs with apostrophes/quotes round-trip safely (i.e.
//     they're HTML-escaped as entities in the attribute value, which
//     the DOM will decode correctly)
//   - enrolled rows still suppress the Select button (no duplicate
//     enrolment affordance)

import (
	"strings"
	"testing"
)

func TestSearchResultsFragment_SelectButtonUsesDataAttrs(t *testing.T) {
	out := SearchResultsFragment([]SearchResult{{
		RedpointID: "cust_abc123",
		Name:       "Alice Smith",
		Email:      "alice@example.com",
		InCache:    false,
	}})

	// The Select button must exist.
	if !strings.Contains(out, ">Select</button>") {
		t.Fatalf("Select button not rendered; output:\n%s", out)
	}

	// Positive contract: data attributes carry the payload.
	if !strings.Contains(out, `data-action="select-member"`) {
		t.Errorf("Select button missing data-action; output:\n%s", out)
	}
	if !strings.Contains(out, `data-redpoint-id="cust_abc123"`) {
		t.Errorf("Select button missing data-redpoint-id; output:\n%s", out)
	}
	if !strings.Contains(out, `data-name="Alice Smith"`) {
		t.Errorf("Select button missing data-name; output:\n%s", out)
	}

	// Negative contract: no inline onclick. This is the specific
	// regression — onclick with single-quoted string literals was the
	// vehicle for the apostrophe-breakage bug.
	if strings.Contains(out, "onclick=") {
		t.Errorf("Select button regressed to inline onclick; output:\n%s", out)
	}
}

func TestSearchResultsFragment_NameWithApostropheRendersSafely(t *testing.T) {
	// "O'Brien" is the exact shape that broke pre-fix. Post-fix it
	// round-trips through HTMLEscape → data attribute value → DOM
	// decode without ever landing inside a JS string literal.
	out := SearchResultsFragment([]SearchResult{{
		RedpointID: "cust_xyz",
		Name:       "Sean O'Brien",
		Email:      "sean@example.com",
		InCache:    false,
	}})

	// The apostrophe must appear HTML-encoded in the attribute, not
	// raw (raw would mean attribute values with ' inside would be fine
	// since the attribute uses ", but we still want the safe encoding
	// as a defense-in-depth).
	if !strings.Contains(out, `data-name="Sean O&#39;Brien"`) {
		t.Errorf("apostrophe in name not HTML-escaped in data-name; output:\n%s", out)
	}

	// Critical: no inline onclick means the old vector is gone even if
	// HTMLEscape's behavior changes.
	if strings.Contains(out, "onclick=") {
		t.Errorf("regressed to onclick — apostrophe-breakage bug would return; output:\n%s", out)
	}
}

func TestSearchResultsFragment_QuoteInIdEscapedSafely(t *testing.T) {
	// Defensive: even unlikely weirdness in the Redpoint ID shouldn't
	// break the button. A literal " character would, pre-fix, have
	// terminated the onclick attribute early.
	out := SearchResultsFragment([]SearchResult{{
		RedpointID: `cust_"exotic"`,
		Name:       "Quoth Raven",
		Email:      "q@example.com",
		InCache:    false,
	}})

	if !strings.Contains(out, `data-redpoint-id="cust_&#34;exotic&#34;"`) {
		t.Errorf("quote in id not HTML-escaped in data-redpoint-id; output:\n%s", out)
	}
	if strings.Contains(out, "onclick=") {
		t.Errorf("regressed to onclick; output:\n%s", out)
	}
}

func TestSearchResultsFragment_EnrolledRowHasNoSelectButton(t *testing.T) {
	// Contract unchanged from pre-fix: once a row is enrolled, we
	// don't show the Select affordance (selecting would just collide
	// with the existing mapping).
	out := SearchResultsFragment([]SearchResult{{
		RedpointID: "cust_abc123",
		Name:       "Alice Smith",
		Email:      "alice@example.com",
		InCache:    true,
		NfcUID:     "04AABB1122",
	}})

	if strings.Contains(out, ">Select</button>") {
		t.Errorf("enrolled row should not render a Select button; output:\n%s", out)
	}
	if !strings.Contains(out, "Enrolled (04AABB1122)") {
		t.Errorf("enrolled badge missing NFC UID; output:\n%s", out)
	}
}

func TestSearchResultsFragment_EmptyResults(t *testing.T) {
	out := SearchResultsFragment(nil)
	if !strings.Contains(out, "No results found") {
		t.Errorf("empty result set should show a friendly placeholder; got:\n%s", out)
	}
}

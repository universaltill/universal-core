package e2e

import (
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// TestErrorToast_RealBrowser is the real-browser proof for the global
// htmx:responseError listener (internal/api/layout.go's shellTmpl doc
// comment): before this existed, a failed htmx request (a validation
// 400, a version-conflict 409) left the page looking exactly like
// nothing happened at all — htmx only swaps a 2xx response into the DOM
// by default, so the click just silently did nothing visible. This test
// drives the real failure path a user hits constantly (submitting a
// form with a required field left blank) through a real browser and
// confirms the toast actually appears with the real server-side error
// message, not a generic placeholder — proving the client-side JSON
// parsing in the shell script genuinely reads the {"data":null,"error":
// "..."} envelope httpx.WriteError produces, not just reacting to the
// failure at all.
func TestErrorToast_RealBrowser(t *testing.T) {
	db := testDB(t)
	withDevAuthEnabled(t)
	srv, tenantID := testServer(t, db)
	ctx := browserCtx(t, tenantID)

	// Item requires both sku and name (purchasing.go) — submitting the
	// new-record form with neither filled in is guaranteed to 400 from
	// the server. formrender's own <input required> (render.go) means a
	// real browser's native constraint validation blocks the click
	// before htmx ever issues the request at all, though — exactly the
	// UX this HTML5 attribute exists for, but not what this test is
	// after (the *server's* error path, once a request actually
	// happens): stripping `required` first forces the click through to
	// a real request/response round trip.
	var dropped bool
	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL+"/forms/Item/new"),
		chromedp.WaitVisible(`form.uc-form`, chromedp.ByQuery),
		chromedp.EvaluateAsDevTools(
			`(function(){document.querySelectorAll('form.uc-form [required]').forEach(function(el){el.removeAttribute('required');}); return true;})()`,
			&dropped,
		),
		chromedp.Click(`form.uc-form button[type="submit"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`.uc-toast-visible`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("submit an invalid form and wait for the error toast: %v", err)
	}

	var toastText string
	if err := chromedp.Run(ctx, chromedp.Text(`#uc-toast`, &toastText, chromedp.ByQuery)); err != nil {
		t.Fatalf("read toast text: %v", err)
	}
	if !strings.Contains(toastText, `"sku" is required`) {
		t.Fatalf(`expected the toast to show the real server validation message ("sku" is required), got %q`, toastText)
	}

	// The form itself must still be there, untouched — a failed
	// validation must never silently wipe out what the user typed (a
	// real risk if htmx had swapped an error response as if it were a
	// normal fragment).
	var stillHasForm bool
	if err := chromedp.Run(ctx, chromedp.EvaluateAsDevTools(
		`document.querySelector('form.uc-form') !== null`, &stillHasForm,
	)); err != nil {
		t.Fatalf("check the form is still present: %v", err)
	}
	if !stillHasForm {
		t.Fatal("expected the form to remain on the page after a failed submission, not be swapped away")
	}
}

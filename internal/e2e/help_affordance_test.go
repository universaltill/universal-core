package e2e

import (
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/form"
)

// TestHelpAffordance_RealBrowser (ADR-0023 in uc-infra, uc-infra#143) is
// the real-browser proof for the "?" help affordance's accessibility
// contract on both a generated form page and a generated list page:
// present, and — even in its disabled state — still focusable and
// keyboard-reachable. A rendered-HTML-string test (see
// TestRender_HelpAffordanceRendersDisabledWhenNoContent in
// internal/kernel/formrender and its listview.go sibling in
// internal/api) proves the markup is there; it can't prove a real
// browser actually keeps the element in the tab order, which is the
// entire point of the explicit tabindex="0" this element carries
// regardless of state — an <a> with no href drops out of the default
// tab order in every current browser without it, so this is exactly the
// kind of "CSS/DOM actually behaves as claimed" gap CLAUDE.md's e2e
// testing rule exists to catch.
//
// Originally exercised this against the real "Item" entity, back when
// no help content shipped for it yet (see internal/help/content/
// README.md at the time). uc-infra#148 — the last of uc-infra#147-152's
// module slices — shipped real content for every entity type in the
// product, including Item, so this test now publishes its own throwaway
// entity+form Definition (publishDef, i18n_text_test.go) under an
// EntityType guaranteed to never have a real help topic, the same fix
// TestRender_HelpAffordanceRendersDisabledWhenNoContent's own doc
// comment describes for its unit-test sibling — proving the disabled-
// state DOM/accessibility contract still needs a page that is genuinely
// undocumented, and there is no longer a real shipped entity that
// qualifies.
func TestHelpAffordance_RealBrowser(t *testing.T) {
	withDevAuthEnabled(t)
	srv, tenantID, tenantDB := testServer(t)
	ctx := browserCtx(t, tenantID)

	fixtureType := "FixtureEntityWithoutHelpTopic"
	publishDef(t, tenantDB,
		&entity.Definition{
			EntityType: fixtureType,
			Version:    1,
			Fields:     []entity.Field{{Name: "name", Type: entity.FieldString, Required: true}},
		},
		&form.Definition{
			EntityType: fixtureType,
			Version:    1,
			Sections: []form.Section{{
				Title:     "Details",
				Component: form.ComponentFields,
				Fields:    []form.FormField{{Name: "name", Label: "Name"}},
			}},
			Actions: []form.Action{{Label: "Save", Op: form.OpSave}},
		},
	)

	cases := []struct {
		name    string
		url     string
		topicID string
	}{
		{"form page", srv.URL + "/forms/" + fixtureType + "/new", "entity/" + fixtureType},
		{"list page", srv.URL + "/records/" + fixtureType, "entity/" + fixtureType},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var present, disabled, noHref, focused, isLinkRole bool
			var topicID string
			if err := chromedp.Run(ctx,
				chromedp.Navigate(c.url),
				chromedp.WaitVisible(`.uc-help-affordance`, chromedp.ByQuery),
				chromedp.EvaluateAsDevTools(
					`document.querySelector('.uc-help-affordance') !== null`, &present,
				),
				chromedp.EvaluateAsDevTools(
					`document.querySelector('.uc-help-affordance').getAttribute('aria-disabled') === 'true'`, &disabled,
				),
				chromedp.EvaluateAsDevTools(
					`!document.querySelector('.uc-help-affordance').hasAttribute('href')`, &noHref,
				),
				// data-help-topic proves the real page's own render call
				// actually derived this entity type's topic id, not just
				// that buildHelpView handles a hand-supplied id correctly
				// in a unit test (independent review, uc-infra#143) — the
				// only other TopicID-derived output, Href, is empty on
				// every page here because fixtureType deliberately has no real content.
				chromedp.AttributeValue(`.uc-help-affordance`, `data-help-topic`, &topicID, nil),
				// role="link" makes the affordance spec-correct in ARIA's
				// accessible-name/state model even in its disabled state
				// (independent review: without it, the element's implicit
				// role while href-less is "generic", on which aria-label
				// is name-prohibited and aria-disabled unsupported per
				// ARIA 1.2 — Chrome currently tolerates this, but the
				// explicit role isn't relying on that tolerance).
				chromedp.EvaluateAsDevTools(
					`document.querySelector('.uc-help-affordance').getAttribute('role') === 'link'`, &isLinkRole,
				),
				// Programmatic .focus() is what actually proves the
				// element is in the tab order in a headless environment
				// — a real Tab keypress would land here too, but only
				// because tabindex="0" put it there; asserting
				// document.activeElement afterward is the DOM-level
				// confirmation a string match on the attribute alone
				// can't give (the attribute could be present and still
				// not work, e.g. on a non-focusable element type).
				chromedp.EvaluateAsDevTools(
					`(function(){
						var el = document.querySelector('.uc-help-affordance');
						el.focus();
						return document.activeElement === el;
					})()`, &focused,
				),
			); err != nil {
				t.Fatalf("%s: drive the page and inspect the help affordance: %v", c.name, err)
			}
			if !present {
				t.Errorf("%s: expected a .uc-help-affordance element, found none", c.name)
			}
			if !disabled {
				t.Errorf("%s: expected aria-disabled=\"true\" — fixtureType has no real help content", c.name)
			}
			if !noHref {
				t.Errorf("%s: expected no href attribute while disabled — a disabled affordance must never link to a 404", c.name)
			}
			if topicID != c.topicID {
				t.Errorf("%s: data-help-topic = %q, want %q", c.name, topicID, c.topicID)
			}
			if !isLinkRole {
				t.Errorf("%s: expected role=\"link\" on the affordance", c.name)
			}
			if !focused {
				t.Errorf("%s: expected the disabled affordance to still be keyboard-focusable (tabindex=\"0\")", c.name)
			}
		})
	}
}

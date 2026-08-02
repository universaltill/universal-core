package api

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"log"
	"net/http"
)

// htmxJS is a vendored, pinned copy of htmx.org 2.0.4
// (https://unpkg.com/htmx.org@2.0.4/dist/htmx.min.js, sha256
// e209dda5c8235479f3166defc7750e1dbcd5a5c1808b7792fc2e6733768fb447) —
// self-hosted rather than loaded from a CDN at runtime, matching this
// kernel's general preference for a minimal, controlled dependency
// footprint (see csvimport.go's own doc comment on the same principle)
// and, more directly, so the app has zero runtime dependency on any
// third-party host being reachable.
//
//go:embed static/htmx.min.js
var htmxJS []byte

// serveHTMX serves the vendored htmx.min.js. Registered unauthenticated
// (outside httpx.DevAuth) in Routes — it's a static asset with no
// tenant-specific content, gating it behind auth would only break the
// page that needs it before auth can even run.
func serveHTMX(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	if _, err := w.Write(htmxJS); err != nil {
		log.Printf("api: serve htmx.min.js: %v", err)
	}
}

// appCSS is the kernel's one global stylesheet — every page (dashboard,
// welcome, forms, list views, the import wizard) shares it via shellTmpl
// below, the same "one shared thing, not per-page styling" reasoning as
// htmxJS. Deliberately plain (no build step, no framework) — see
// static/app.css's own header comment.
//
//go:embed static/app.css
var appCSS []byte

// appCSSPath embeds a content hash into app.css's own URL
// (/static/app-{hash}.css), computed once at process start. Unlike
// htmxJS (a pinned, rarely-changing vendored file, where a stable
// filename + immutable cache header is correct), app.css changes on
// almost every deploy during active development — serving it from a
// fixed "/static/app.css" URL with a one-year immutable cache header
// (the bug this fixed) meant a browser that had ever loaded the page
// before kept serving a stale, pre-hub, pre-i18n-switcher stylesheet
// for up to a year, never even revalidating: exactly the "no circles,
// just plain text" symptom Farshid saw. Baking the hash into the URL
// means every content change is automatically a new URL, so the long
// immutable cache header becomes safe again rather than the bug.
var appCSSPath = func() string {
	sum := sha256.Sum256(appCSS)
	return "/static/app-" + hex.EncodeToString(sum[:])[:12] + ".css"
}()

// serveCSS serves the vendored app.css at its content-hashed path (see
// appCSSPath). Registered unauthenticated, same reasoning as serveHTMX:
// a static asset with no tenant-specific content, needed before auth
// can even render an error page.
func serveCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	if _, err := w.Write(appCSS); err != nil {
		log.Printf("api: serve app.css: %v", err)
	}
}

// shellTmpl wraps a page fragment in the minimal HTML document a real
// browser needs to actually run htmx: without a real <script> tag
// loading htmx.js somewhere, every hx-* attribute this kernel's
// templates render (formrender, the import wizard) is inert markup — a
// browser has no code to interpret them. Found by internal/e2e's first
// real-browser test: every "verified end-to-end" claim before that test
// existed was verified via curl, which doesn't execute JavaScript and
// so could never have caught this — the fragments were correct, nothing
// ever loaded the runtime that makes hx-post/hx-get/hx-target work.
//
// Only wraps the *first* page a browser navigates to for a given
// entityType (renderForm, importUploadPage) — every htmx-swap response
// (importPreview, importCommit, createRecord, etc.) must keep returning
// a bare fragment, since htmx replaces an existing element's innerHTML/
// outerHTML with exactly that response; wrapping a swap response in a
// full <html> document would break the swap, not fix anything.
//
// Nav is pre-rendered HTML (see nav.go's renderNav) so shellTmpl itself
// never needs to know about tenants/modules/auth state — it's just
// layout, same separation formrender already keeps between rendering
// and the registry lookups that feed it.
//
// lang/dir on <html> is not cosmetic: without dir="rtl" for Arabic, the
// page is still laid out left-to-right underneath Arabic text — i18n
// strings translated but the surrounding layout still reading the wrong
// direction is arguably worse than not translating at all. See
// locale.go's localeDir.
//
// The trailing <script> is a global htmx:responseError/htmx:sendError
// listener — every htmx-driven form submission or button in this kernel
// (formrender's own <form>, the import wizard, the workflow inbox's
// Approve button, the AI-provider settings form) used to fail
// completely silently on a non-2xx response: htmx only swaps a 2xx
// response into the DOM by default, so a validation 400, a version-
// conflict 409, or a network failure just... did nothing visible, the
// user's click appeared to do nothing at all (QUEUE.md flagged this
// directly: "a visible error/toast on a version conflict or failed
// approve in the htmx UI"). Wired once, here, rather than per-page:
// every page already goes through this one shell, so a single
// body-level listener (htmx:responseError bubbles from whatever element
// made the failing request up to document.body — see htmx's own event
// docs) covers every current and future htmx surface in this kernel
// with no per-page opt-in needed, the same "one shared thing" reasoning
// appCSS/htmxJS already apply to CSS/JS themselves.
var shellTmpl = template.Must(template.New("shell").Parse(fmt.Sprintf(`<!doctype html>
<html lang="{{.Lang}}" dir="{{.Dir}}">
<head>
<meta charset="utf-8">
<link rel="stylesheet" href="%s">
<script src="/static/htmx.min.js"></script>
</head>
<body>
{{.Nav}}
<main class="uc-container">
{{.Body}}
</main>
<dialog id="uc-quick-create" class="uc-modal">
<div class="uc-modal-header">
<button type="button" id="uc-quick-create-close" class="uc-modal-close" aria-label="{{.QuickCreateCancel}}">×</button>
</div>
<div id="uc-quick-create-body" class="uc-modal-body"></div>
</dialog>
<script>
(function() {
  // Stamp every htmx request with the tenant this PAGE was rendered
  // for (the nav's data-uc-tenant, set from the request context) so a
  // stale tab can't silently mutate whichever tenant the shared
  // session cookie has since switched to — the server refuses a
  // mismatch with 409 (guardTenantHeader, ADR-0011 in uc-infra).
  document.body.addEventListener("htmx:configRequest", function(evt) {
    var nav = document.querySelector("[data-uc-tenant]");
    if (nav) {
      evt.detail.headers["X-UC-Tenant"] = nav.getAttribute("data-uc-tenant");
    }
  });
  var toastEl = null;
  function ensureToast() {
    if (toastEl) { return toastEl; }
    toastEl = document.createElement("div");
    toastEl.id = "uc-toast";
    toastEl.className = "uc-toast";
    toastEl.setAttribute("role", "alert");
    document.body.appendChild(toastEl);
    return toastEl;
  }
  function showToast(message) {
    var el = ensureToast();
    el.textContent = message;
    el.classList.add("uc-toast-visible");
    window.clearTimeout(el._ucHideTimer);
    el._ucHideTimer = window.setTimeout(function() {
      el.classList.remove("uc-toast-visible");
    }, 6000);
  }
  document.body.addEventListener("htmx:responseError", function(evt) {
    var xhr = evt.detail && evt.detail.xhr;
    var message = {{.ToastFallback}};
    if (xhr && xhr.responseText) {
      try {
        var body = JSON.parse(xhr.responseText);
        if (body && body.error) { message = body.error; }
      } catch (e) {
        // Not a JSON envelope (e.g. a plain-text 401/413 body) — the
        // generic fallback message stands.
      }
    }
    showToast(message);
  });
  document.body.addEventListener("htmx:sendError", function() {
    showToast({{.ToastNetworkError}});
  });

  // Global console/error capture for the in-app issue logger
  // (internal/api/issuereport.go, universaltill/uc-infra#46): every page
  // already goes through this one shell, so wiring this here — same
  // reasoning as the htmx error-toast listener above — means the issue
  // report page can pick up what happened on whichever page the problem
  // actually occurred on, not just its own. Persisted to sessionStorage
  // (tab lifetime only, not localStorage) rather than sent anywhere; the
  // issue-report page is the only reader, and only once a human has
  // reviewed — and can still edit/redact — it in a visible textarea
  // before Submit (not readonly, unlike the voice transcript: that field
  // is the user's own dictated speech, this one is machine-harvested
  // content they never authored and may need to strike something from).
  // Capped so an unbounded run of console spam can't grow this without
  // bound; oldest entries drop first. Every listener body is wrapped in
  // try/catch: a bug in this capture code must never be able to break
  // the app it's supposed to be helping debug.
  //
  // Keyed per-tenant (ucTenantKey below), not one bare origin-wide key:
  // sessionStorage survives an in-tab tenant switch
  // (guardTenantHeader/X-UC-Tenant above is exactly why that's possible)
  // and a same-tab logout/login, and this kernel's multi-tenancy rule
  // (universal-core/CLAUDE.md) treats cross-tenant content mixing as the
  // single most consequential thing to avoid — Tenant A's console output
  // must never end up pre-filled into a report filed against Tenant B.
  // issueReportSubmit's own script clears this tenant's key once a
  // submission is underway, so a filed report's content isn't silently
  // resubmitted/inflated by a second report in the same session.
  //
  // Single-quoted string literals below ('error', not "error") are
  // deliberate, not stylistic: this script is rendered into every page's
  // body, and TestAPI_RBAC_DeniedPageIsLocalized (and friends) assert a
  // rendered page never contains the double-quoted substring error, as a
  // check against accidentally serving the raw JSON envelope instead of
  // a page. A double-quoted "error" here would trip that check on every
  // page, not just the denial page.
  function ucTenantKey() {
    var nav = document.querySelector("[data-uc-tenant]");
    return "ucConsoleLog:" + (nav ? nav.getAttribute("data-uc-tenant") : "anonymous");
  }
  var ucLogMaxEntries = 200;
  var ucLogMaxChars = 20000;
  function ucAppendLog(entry) {
    try {
      var key = ucTenantKey();
      var buf;
      try {
        buf = JSON.parse(window.sessionStorage.getItem(key) || "[]");
        if (!Array.isArray(buf)) { buf = []; }
      } catch (e) {
        buf = [];
      }
      buf.push(entry);
      while (buf.length > ucLogMaxEntries) {
        buf.shift();
      }
      while (buf.join("\n").length > ucLogMaxChars && buf.length > 1) {
        buf.shift();
      }
      window.sessionStorage.setItem(key, JSON.stringify(buf));
    } catch (e) {
      // sessionStorage unavailable (private mode, quota, etc.) — capture
      // is best-effort, never worth surfacing to the user.
    }
  }
  try {
    ['error', 'warn'].forEach(function(level) {
      var original = console[level];
      console[level] = function() {
        try {
          var parts = Array.prototype.slice.call(arguments).map(String);
          ucAppendLog("[" + level + "] " + parts.join(" "));
        } catch (e) {
          // never let capture break the original console call below.
        }
        if (original) { original.apply(console, arguments); }
      };
    });
    window.addEventListener('error', function(evt) {
      try {
        var msg = evt && evt.message ? evt.message : "window error";
        var loc = evt && evt.filename ? (" (" + evt.filename + ":" + evt.lineno + ")") : "";
        ucAppendLog("[error] " + msg + loc);
      } catch (e) {
        // see ucAppendLog's own try/catch note above.
      }
    });
    window.addEventListener("unhandledrejection", function(evt) {
      try {
        var reason = evt && evt.reason ? String(evt.reason) : "unhandled rejection";
        ucAppendLog("[unhandledrejection] " + reason);
      } catch (e) {
        // see ucAppendLog's own try/catch note above.
      }
    });
  } catch (e) {
    // Installing the capture itself must never be able to take down
    // everything registered after it in this same IIFE (the reference-
    // field combobox below) — e.g. some environment where console is
    // missing or non-configurable. Capture is simply absent in that case.
  }

  // Searchable reference-field combobox (#24). A reference field renders
  // as .uc-ref { hidden input (the id), text input (the search), results
  // div }. Typing queries /api/references/{target}; clicking an option
  // sets the hidden id and the visible label. Delegated from the body so
  // it works for fields swapped in by htmx too, and debounced so a fast
  // typist doesn't fire a request per keystroke.
  var refTimers = new WeakMap();
  // refSeq gives each search box a monotonic request counter. Every fetch
  // captures the counter's value at dispatch and its response is applied
  // only if it is still the latest — so a slow earlier request can't
  // overwrite a fresher one's results with stale ones, and picking an
  // option (which bumps the counter) discards any still-in-flight fetch
  // rather than letting it reopen the dropdown a moment later.
  var refSeq = new WeakMap();
  document.body.addEventListener("input", function(evt) {
    var search = evt.target;
    if (!search.classList.contains("uc-ref-search")) { return; }
    var box = search.closest(".uc-ref");
    var hidden = box.querySelector('input[type="hidden"]');
    // Editing the search text invalidates any previously chosen id until
    // a new option is clicked — otherwise the label and the id could
    // disagree and the form would submit a stale reference.
    hidden.value = "";
    window.clearTimeout(refTimers.get(search));
    refTimers.set(search, window.setTimeout(function() {
      var mySeq = (refSeq.get(search) || 0) + 1;
      refSeq.set(search, mySeq);
      var url = "/api/references/" + encodeURIComponent(box.dataset.target)
              + "?q=" + encodeURIComponent(search.value);
      // Field.TargetFilter/MustMatchParentField (uc-infra#78): tell the
      // server WHICH reference field is searching, so it can apply that
      // field's own declared constraint — a target entity type (e.g.
      // Party) can be the target of several different fields with
      // different constraints (TimeEntry.employee_id vs
      // SalesOrder.customer_id), so the target type alone isn't enough.
      var form = box.closest(".uc-form");
      if (form) {
        url += "&source_entity_type=" + encodeURIComponent(form.dataset.entityType)
             + "&source_field=" + encodeURIComponent(box.dataset.field);
        // MustMatchParentField: read the CURRENT value of the sibling
        // field the Definition named (data-must-match-field, set only
        // when the field declares one) directly off this same form, so
        // the server can filter candidates to ones sharing that value —
        // e.g. only Tasks in the same project as the one being edited.
        if (box.dataset.mustMatchField) {
          var sibling = form.querySelector('[name="' + box.dataset.mustMatchField + '"]');
          if (sibling && sibling.value) {
            url += "&sibling_value=" + encodeURIComponent(sibling.value);
          }
        }
      }
      fetch(url, { headers: { "Accept": "application/json" } })
        .then(function(r) { return r.json(); })
        .then(function(env) {
          if (refSeq.get(search) !== mySeq) { return; } // superseded
          var results = box.querySelector(".uc-ref-results");
          var opts = (env && env.data) || [];
          results.innerHTML = "";
          opts.forEach(function(o) {
            var el = document.createElement("div");
            el.className = "uc-ref-option";
            el.textContent = o.label;
            el.setAttribute("data-id", o.id);
            results.appendChild(el);
          });
          results.hidden = opts.length === 0;
        })
        .catch(function() { /* a failed search leaves the last results up */ });
    }, 200));
  });
  document.body.addEventListener("click", function(evt) {
    var opt = evt.target;
    if (!opt.classList.contains("uc-ref-option")) { return; }
    var box = opt.closest(".uc-ref");
    var search = box.querySelector(".uc-ref-search");
    // Cancel any pending/in-flight search for this box so it can't reopen
    // the results right after a selection.
    window.clearTimeout(refTimers.get(search));
    refSeq.set(search, (refSeq.get(search) || 0) + 1);
    box.querySelector('input[type="hidden"]').value = opt.getAttribute("data-id");
    search.value = opt.textContent;
    box.querySelector(".uc-ref-results").hidden = true;
  });

  // Inline reference-picker quick-create (part 2 of #24). A .uc-ref box
  // whose viewer holds create permission on its target (server-decided,
  // formrender/render.go's CreateNewLabel) renders a ".uc-ref-create"
  // button. Clicking it opens the target entity's OWN generated create
  // form (GET /forms/{target}/new, fetched as a bare fragment via
  // isHTMXRequest — see handlers.go's renderForm) inside a native
  // <dialog>, so the picker's own parent form is never navigated away
  // from and loses none of its in-progress edits.
  var quickCreateBox = null; // the .uc-ref that opened the dialog, so a
                              // successful create knows which picker to fill.
  var quickCreateIDPrefix = "uc-quick-create-field-";
  document.body.addEventListener("click", function(evt) {
    var btn = evt.target;
    if (!btn.classList.contains("uc-ref-create")) { return; }
    // One quick-create at a time: without this, clicking a SECOND
    // picker's button before the first's fetch resolves races two
    // in-flight requests against a single shared quickCreateBox (found
    // by this feature's own independent review — whichever response
    // lands last silently wins, and the created record can be written
    // into the WRONG picker), and clicking a quick-create button that
    // happens to be nested inside the dialog's own fetched form (e.g.
    // Department.parent_department_id, self-referencing) would
    // reassign quickCreateBox and immediately detach it via the
    // body.innerHTML reset two lines below, silently discarding
    // whatever the outer quick-create was doing. Ignoring the click
    // while one is already open/in-flight closes both holes at once.
    if (quickCreateBox) { return; }
    quickCreateBox = btn.closest(".uc-ref");
    var dialog = document.getElementById("uc-quick-create");
    var body = document.getElementById("uc-quick-create-body");
    body.innerHTML = "";
    var nav = document.querySelector("[data-uc-tenant]");
    fetch("/forms/" + encodeURIComponent(btn.getAttribute("data-target")) + "/new", {
      headers: {
        "HX-Request": "true",
        "X-UC-Tenant": nav ? nav.getAttribute("data-uc-tenant") : ""
      }
    })
      .then(function(r) {
        if (!r.ok) { throw new Error("quick-create form fetch failed: " + r.status); }
        return r.text();
      })
      .then(function(html) {
        body.innerHTML = html;
        // Every id/for in the fetched fragment is rewritten with a
        // fixed prefix before it ever touches the live document: the
        // fragment is formrender's normal output, which names every
        // input id="{fieldName}" — identical to whatever the OUTER
        // form (still in the DOM behind the modal) already used for
        // its own fields of the same name. Without this,
        // <label for="code"> inside the modal resolves, in document
        // order, to the OUTER form's (inert, hidden-behind-the-dialog)
        // input instead of the modal's own — clicking a label in the
        // quick-create modal would silently focus nothing useful, and
        // assistive tech would compute the wrong accessible name
        // (found by this feature's own independent review).
        body.querySelectorAll("[id]").forEach(function(el) {
          el.id = quickCreateIDPrefix + el.id;
        });
        body.querySelectorAll("[for]").forEach(function(el) {
          el.setAttribute("for", quickCreateIDPrefix + el.getAttribute("for"));
        });
        // The fetched markup carries hx-post/hx-target attributes that
        // only take effect once htmx has scanned them — a plain innerHTML
        // assignment does not trigger htmx's own mutation observer for
        // content it didn't insert itself.
        htmx.process(body);
        dialog.showModal();
      })
      .catch(function() {
        quickCreateBox = null; // release the one-at-a-time guard above —
                                // the dialog never opened, so there is
                                // nothing to close.
        showToast({{.ToastFallback}});
      });
  });
  // The single place quick-create's own DOM state gets torn down.
  // Idempotent (safe to call more than once, or when already closed) —
  // it is called directly, SYNCHRONOUSLY, by every close path this code
  // itself triggers (×, backdrop, successful create, below), and ALSO
  // wired to the dialog's native "close" event as a safety net purely
  // for Escape, which closes a <dialog> through the browser's own
  // internal algorithm and never runs any of this code otherwise. Those
  // two are NOT redundant in the way they look: a <dialog>'s "close"
  // event is fired via a QUEUED TASK, not synchronously, even for our
  // own explicit .close() calls (confirmed the hard way — an earlier
  // version of this fix relied on the event alone for every path, which
  // made the real-browser test for the successful-create path flaky,
  // since it asserts the dialog body is cleared immediately after
  // create resolves, before that queued task has necessarily run). The
  // synchronous direct calls keep every path this code controls
  // deterministic; the event listener only ever does extra, harmless
  // work for those, and is the sole cleanup for the one path (Escape)
  // this code cannot intercept directly.
  function closeQuickCreate() {
    var dialog = document.getElementById("uc-quick-create");
    if (dialog.open) { dialog.close(); }
    document.getElementById("uc-quick-create-body").innerHTML = "";
    quickCreateBox = null;
  }
  document.getElementById("uc-quick-create").addEventListener("close", closeQuickCreate);
  document.getElementById("uc-quick-create-close").addEventListener("click", closeQuickCreate);
  document.getElementById("uc-quick-create").addEventListener("click", function(evt) {
    // A <dialog>'s own backdrop is the dialog element itself outside its
    // content box — a click landing directly on it (not on anything it
    // contains) is a backdrop click.
    if (evt.target.id === "uc-quick-create") { closeQuickCreate(); }
  });
  // Listens on htmx:afterSettle, not htmx:afterSwap: htmx dispatches
  // afterSettle on the swapped element itself, and a handler here that
  // removed the element from the DOM (closeQuickCreate below clears
  // #uc-quick-create-body's innerHTML synchronously) would detach it
  // before that dispatch — a detached node's dispatched event never
  // bubbles up to this document.body listener, since bubbling requires
  // the live ancestor chain. Reacting on afterSettle instead means htmx
  // is fully done with the element by the time this runs, so removing
  // it here is safe.
  document.body.addEventListener("htmx:afterSettle", function(evt) {
    var quickCreateBody = document.getElementById("uc-quick-create-body");
    if (!quickCreateBody || !quickCreateBody.contains(evt.detail.elt)) { return; }
    // Any swap reaching the modal body IS a successful save: htmx's
    // default responseHandling only swaps on a 2xx response (4xx/5xx
    // trigger htmx:responseError instead, already handled by the global
    // toast listener above) — a validation failure never reaches here.
    var savedForm = quickCreateBody.querySelector("form.uc-form[data-record-id]");
    if (!savedForm || !quickCreateBox) { return; }
    var hidden = quickCreateBox.querySelector('input[type="hidden"]');
    var search = quickCreateBox.querySelector(".uc-ref-search");
    hidden.value = savedForm.getAttribute("data-record-id");
    search.value = savedForm.getAttribute("data-record-label") || hidden.value;
    quickCreateBox.querySelector(".uc-ref-results").hidden = true;
    closeQuickCreate();
  });
})();
</script>
</body>
</html>
`, appCSSPath)))

type shellView struct {
	Lang string
	Dir  string
	Nav  template.HTML
	Body template.HTML
	// ToastFallback/ToastNetworkError feed the global error-toast script
	// above — translated via h.catalog, same as every other user-facing
	// string in this kernel (CLAUDE.md's "no hardcoded user-facing
	// strings" rule applies just as much to this JS-embedded text as to
	// any Go-rendered HTML).
	ToastFallback     string
	ToastNetworkError string
	// QuickCreateCancel labels the reference-picker quick-create dialog's
	// close button (part 2 of #24, aria-label only — the button itself is
	// a plain "×") — same "translate every JS-embedded string" reasoning
	// as ToastFallback above.
	QuickCreateCancel string
}

// renderShell writes fragment wrapped in shellTmpl, with nav as the
// page's top chrome and locale driving the document's lang/dir. nav and
// fragment are already-rendered, already-escaped HTML (nav from
// renderNav, fragment from formrender/importTmpl/dashboardTmpl, all
// html/template output), not raw user input — passed as template.HTML
// deliberately, the same trust boundary formrender's own Render already
// crossed once for this exact content. A Handler method (not a free
// function) specifically so it can look up the toast strings via
// h.catalog itself, rather than every one of its ~10 callers needing to
// pass two more translated strings through their own signature for
// something that has nothing to do with what each of those pages is
// actually rendering.
func (h *Handler) renderShell(w http.ResponseWriter, locale string, nav, fragment template.HTML) error {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	view := shellView{
		Lang:              locale,
		Dir:               localeDir(locale),
		Nav:               nav,
		Body:              fragment,
		ToastFallback:     h.catalog.T(locale, "toast.error_fallback"),
		ToastNetworkError: h.catalog.T(locale, "toast.network_error"),
		QuickCreateCancel: h.catalog.T(locale, "action.cancel"),
	}
	if err := shellTmpl.Execute(w, view); err != nil {
		return fmt.Errorf("render page shell: %w", err)
	}
	return nil
}

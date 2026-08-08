package moduleseed_test

// fingerprint_test.go is the regression gate for uc-infra#117:
// moduleseed.publishOne's status-guard (moduleseed.go:82 — "if status ==
// data.StatusPublished || status == data.StatusRolledBack { return
// nil }") no-ops on any (key, version) already published — it never
// diffs the stored JSON against what the module code would publish
// today. uc-infra#66 and #69 both correctly bumped a Form Definition's
// Version when editing it; nothing in the test suite would have caught
// it if they hadn't, and a Definition edited with no Version bump
// silently never reaches an already-provisioned tenant. This test
// closes that gap at build time: it hashes every module's
// currently-published Entity/Form Definitions and fails the moment a
// (EntityType/FormType, Version) pair's content drifts from the
// committed baseline below, without a matching Version bump to give it
// a new key — and, symmetrically, fails if a Definition present in the
// baseline stops being produced at all (TestDefinitionFingerprints'
// trailing count check), so a dropped/renamed Definition can't silently
// disappear from wantHashes unnoticed either.
//
// Enumeration takes entities straight from
// internal/modules.Publishers[key].Definitions plus
// modules.FoundationDefinitions() — that registry (modules.go's own doc
// comment) exists specifically so nothing needs "a third list to keep
// in sync with this map," and a hand-written module list here would
// have been exactly that fourth list, with no parity guard able to
// catch a *wrong* entry (only a wrong *count*). Forms have no such
// registry accessor, so formDefSets below mirrors
// internal/kernel/formrender/i18n_coverage_test.go's moduleFormSets
// convention instead (hand-written list, "+1 for foundation" parity
// guard against len(modules.Publishers)) — same external _test package
// shape that file already establishes (avoids an import cycle:
// purchasing/sales/etc. already import moduleseed in production, so
// this file importing them back is the standard external-test-package
// direction, not a cycle).
//
// wantHashes is NOT a retroactive audit of already-shipped Definition
// history (explicit non-goal) — it is a snapshot of what every module
// publishes today, taken when this test was added, and the new
// enforcement baseline from this point forward. A failing "no baseline
// hash for key ..." error on a legitimate Version bump is expected and
// is not a bug: copy the printed ready-to-paste line into wantHashes as
// part of that same commit (and delete the superseded version's line —
// the trailing count check in TestDefinitionFingerprints holds
// wantHashes to containing exactly what's live, so an orphaned entry
// left behind by a bump fails loudly too), the same way a golden-file
// update normally works, just without separate regen tooling — at ~120
// entries a single literal is easier to review in a diff than a
// generated fixture file.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/universaltill/universal-core/internal/kernel/assets"
	"github.com/universaltill/universal-core/internal/kernel/crm"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/finance"
	"github.com/universaltill/universal-core/internal/kernel/form"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
	"github.com/universaltill/universal-core/internal/kernel/hr"
	"github.com/universaltill/universal-core/internal/kernel/projects"
	"github.com/universaltill/universal-core/internal/kernel/purchasing"
	"github.com/universaltill/universal-core/internal/kernel/sales"
	"github.com/universaltill/universal-core/internal/modules"
)

// entityDefSets is driven directly by modules.Publishers +
// modules.FoundationDefinitions() (this file's top doc comment) — no
// hand-written module list, so no way for this half to drift from what
// internal/modules actually knows about; a future module registered in
// Publishers is picked up automatically.
func entityDefSets() [][]*entity.Definition {
	sets := [][]*entity.Definition{modules.FoundationDefinitions()}
	for _, p := range modules.Publishers {
		sets = append(sets, p.Definitions())
	}
	return sets
}

// formDefSets mirrors i18n_coverage_test.go's moduleFormSets exactly.
func formDefSets() [][]*form.Definition {
	return [][]*form.Definition{
		foundation.AllForms(),
		sales.AllForms(),
		projects.AllForms(),
		crm.AllForms(),
		hr.AllForms(),
		assets.AllForms(),
		finance.AllForms(),
		purchasing.AllForms(),
	}
}

// TestModuleFingerprintSets_CoversEveryRegisteredModule is what makes
// the gate below binding on a FUTURE module, the same reason
// formrender's TestModuleFormSets_CoversEveryRegisteredModule exists:
// without it, a ninth module could register in modules.Publishers and
// formDefSets (still a hand-written list — forms have no registry
// accessor to derive from, unlike entityDefSets above) would keep
// passing having never looked at it. Entity coverage needs no such
// guard: entityDefSets is built directly from modules.Publishers, so it
// covers every registered module by construction, not by a count that
// could itself go stale.
func TestModuleFingerprintSets_CoversEveryRegisteredModule(t *testing.T) {
	// +1 for foundation, which is not in Publishers (modules.FoundationKey's
	// doc comment: every tenant has it unconditionally, so it's never an
	// opt-in module key).
	if got, want := len(formDefSets()), len(modules.Publishers)+1; got != want {
		t.Fatalf("formDefSets lists %d modules but internal/modules knows %d (%d optional + foundation): "+
			"a module missing here is a module whose Form Definitions this fingerprint gate silently never checks",
			got, want, len(modules.Publishers))
	}
}

// fingerprint hashes v the same way it would actually be published:
// json.Marshal is exactly what every module's Publish/PublishForms
// calls before handing bytes to moduleseed.Item.Raw (see e.g.
// purchasing/seed.go), so this test observes precisely the content that
// would (or wouldn't) reach a tenant's registry, not some parallel
// notion of "the definition" that could drift from the real publish
// path.
func fingerprint(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// TestDefinitionFingerprints is the actual gate: every module's
// published Entity/Form Definition, keyed by (kind, EntityType,
// Version) — kind-prefixed because one EntityType can have both an
// entity.Definition and a form.Definition (e.g. "PurchaseOrder") — must
// hash to exactly what's recorded in wantHashes below. A mismatch on an
// EXISTING key means the Definition's content changed without its
// Version changing too — the silent-no-op bug uc-infra#117 exists to
// catch. A key with no entry at all means either a genuine new Version
// (expected: paste the printed line into wantHashes) or a brand new
// Definition (same fix).
func TestDefinitionFingerprints(t *testing.T) {
	checked := 0
	for _, defs := range entityDefSets() {
		for _, def := range defs {
			key := fmt.Sprintf("entity:%s@v%d", def.EntityType, def.Version)
			hash, err := fingerprint(def)
			if err != nil {
				t.Fatalf("%s: marshal entity definition: %v", key, err)
			}
			checkFingerprint(t, key, hash)
			checked++
		}
	}
	for _, forms := range formDefSets() {
		for _, f := range forms {
			key := fmt.Sprintf("form:%s@v%d", f.EntityType, f.Version)
			hash, err := fingerprint(f)
			if err != nil {
				t.Fatalf("%s: marshal form definition: %v", key, err)
			}
			checkFingerprint(t, key, hash)
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no Entity/Form Definitions found across any module's All()/AllForms() — entityDefSets/formDefSets are stale or every module returned nothing, either way this test isn't checking anything")
	}
	// The loops above only ever check "does this produced key match its
	// baseline" — never the converse. Without this, a Definition (or a
	// whole module) that stops being produced — dropped from
	// All()/AllForms(), a module deleted from modules.Publishers, a
	// constructor accidentally omitted from a return list — just leaves
	// its wantHashes entry unvisited, and the test stays green having
	// silently stopped checking it. checked counts every key actually
	// produced this run; wantHashes' own length is the complete set this
	// test is supposed to be enforcing, so the two must match exactly —
	// fewer means something disappeared, more is impossible (every
	// checked key not already fatal-erroring above requires a wantHashes
	// hit).
	if checked != len(wantHashes) {
		t.Errorf("checked %d Definitions this run but wantHashes has %d entries — a Definition (or a whole module) that used to be "+
			"fingerprinted is no longer being produced by any module's All()/AllForms(); if that's intentional, remove its now-stale "+
			"line(s) from wantHashes in the same commit", checked, len(wantHashes))
	}
}

func checkFingerprint(t *testing.T, key, hash string) {
	t.Helper()
	want, ok := wantHashes[key]
	if !ok {
		t.Errorf("no baseline hash for key %q — if this is a genuine new Definition or Version bump, add this line to wantHashes in fingerprint_test.go:\n\t%q: %q,", key, key, hash)
		return
	}
	if want != hash {
		t.Errorf("%s: content changed without a Version bump (want hash %s, got %s) — either bump this Definition's Version "+
			"(and add its new key to wantHashes), or if the content wasn't meant to change, revert it", key, want, hash)
	}
}

// wantHashes is the committed baseline described in this file's top
// doc comment — one entry per (kind, EntityType, Version) currently
// published by any module, captured from the actual repo content, not
// invented. TestDefinitionFingerprints' trailing count check makes
// "currently published" an enforced invariant, not just a description:
// an entry left behind after its Definition is dropped or superseded by
// a new Version fails that check. Regenerate a specific entry by
// running this test with -v: a failing "no baseline hash" error prints
// the exact line to paste.
var wantHashes = map[string]string{
	"entity:Party@v3":                        "ae9b08dd727881048e5958165315ab05657caa98cbd28c95e5db6bd13540a277",
	"entity:PartyRole@v3":                    "67589ddb98a50f064830189edf6e7b29be0c22fcc7f922fda8663ba58e5355d1",
	"entity:PartyRelationship@v3":            "940b2b135892b4b9b2499875527ad73cb25357f0d762f9ad30a6f9dc8ad1d931",
	"entity:Address@v2":                      "9f9f42d98d22911e54546155c16cc4b8157e8480092239e947403b892f54b911",
	"entity:ContactMechanism@v2":             "d826b27619304bff387ef3ac3b85554853d1ab23a862faadaeb742b1cb7ce6b1",
	"entity:Attachment@v3":                   "349d13430e9580e209853a6517b06acaec6d75df9c49c5e1700f59ee5b8b1c6c",
	"entity:UnitOfMeasure@v2":                "fea029ba7af041ab7119bb1dee4a1481a478739e180b08b69fc14a3a75f374df",
	"entity:UomConversion@v3":                "a33fa5d05a1a09cd7b766f639587dd537ae16a42c80f7caf312cc167955a07d3",
	"entity:Currency@v4":                     "11be2555483ce63cbeae04c70bb885c9c352a341ecad5bf0005819c58d1ca50a",
	"entity:ExchangeRate@v3":                 "78516fa2b76628e40b05b92ac2bf4c25b674fd68b139605a7ac0e58a774beea7",
	"entity:StatusType@v1":                   "633c522a100ada70a2fb5aa281ba24577730a5aead9bcac43d91da4b70f1a054",
	"entity:Status@v1":                       "53b8108a33de9c1104fbf088c8a2aa7dcd81127a4a1f0a305e095e9f3d3acdb3",
	"entity:StatusTransition@v1":             "00f369446a59120ff712e191e65b8ab3fea4084a8fafad3a8dd437ec75921248",
	"entity:IssueReport@v2":                  "17734a0048f334366ab605545896e6f9a6909eed9eddd8410948e651c654399f",
	"entity:AIProviderConnection@v1":         "dc50961ebab9cadfb45ab2a47ca7552b7b3c97c6c3c6a56a94bd1a8fe8ac41f7",
	"entity:ExternalSQLSource@v1":            "837549058fc96b76c07441fa4e809e926d09e42fc21307688379df88cb21d1dd",
	"entity:ExternalIdentity@v2":             "2b6b1ec11b587105e92d334cc497476e42a55e4077b4224cfc094ab6bce4a40b",
	"entity:SystemOfRecord@v2":               "42ebcc1629330b30ce60fcb8b5a0f66b5fba934b7c8dbe142c4181dd3e64e22a",
	"entity:Role@v1":                         "a5ecb6826f637562983af81c4728017f6d3da40a5345b61c3242db90565cf415",
	"entity:UserRole@v2":                     "7e7c9e74b5f635797d05a4f61fca4d09b10fdd2bded45bae07acac632635ef35",
	"entity:Permission@v1":                   "cc19972991d84c3f86bdbd044ce6d9779fc89b22f13369d53b55beba1583625d",
	"entity:FieldPermission@v1":              "77d6eaaa5868dad92dde31eedb0eb40b7bbb0cbf87b2d56a5a2fb29d29e85c6a",
	"entity:Department@v1":                   "9d566a5fd662da32728dbeb58adca4418654141dac02aa2d1450738fa071cf07",
	"entity:Position@v1":                     "19d81db7900a09df4cb8f488d644f03d0dfe9c700e987e08cab58752c0dfa3c4",
	"entity:Delegation@v1":                   "89577b2819e57c98f71b7ee389fa22832159668d45d53de1ea1cb649884d9373",
	"entity:SalesOrder@v3":                   "8cbe23b99b23677ae54b6751dc3c40e52c26510a725d26e9114fc0273ca2096b",
	"entity:SOLine@v2":                       "6f8199dc56da89ff524665539d0ea415de097f8ca2e78bc4ee55873989f4c699",
	"entity:CustomerInvoice@v2":              "cc04eaf01b64fc072eda425b74e9638de41193413fcf2a420160b1ee65c3088c",
	"entity:Project@v2":                      "3730107914692d8bc29eef03d7c9d8359b35ceee65692346f5facc7987247c10",
	"entity:Task@v3":                         "fd4f5e3ca00fc3feb2a6be85c7be5789584f64dd4396c0f57deb65f1370235fc",
	"entity:TimeEntry@v3":                    "aed386436ebf022c3a15b60a66d45c6c23657c21b31b65f6fd5a0038812e69db",
	"entity:ProjectBudgetLine@v1":            "aa1a9d9cca064f57637d829078fb96f12b9244e72d87c442b473da64deccefef",
	"entity:Case@v1":                         "2c80a3934eb628d8ffc7362e509a78b8b06973cbe22d610a8ef983086589b3ef",
	"entity:Campaign@v2":                     "97bb5a324dbce73d8018f183b083504db5e9d8320b274ec8efee2895a73d0b02",
	"entity:Lead@v1":                         "78104365bfc309165e5babc3f33ff01d10aa5302f2c999f43957f14c8173d2c4",
	"entity:Opportunity@v2":                  "b31dd31b83a6ca1086e21df5e9e7a1e5e0c2416001f25dbea14bcae917d1d0be",
	"entity:Employee@v2":                     "f9c9f3753f2bd8a43370411e81f6d7b1d12be15126dbfd6499e271681f8ed625",
	"entity:LeaveRequest@v2":                 "02dbe44da3221e2e2ce87c5bd7ed03fdf793789c8b61a45c7e5cd141ded59033",
	"entity:AttendanceRecord@v2":             "060748ee86183d463343a7ee8a3d29c30e8f5cd6af3aac0131f0477359bd0a4a",
	"entity:FixedAsset@v3":                   "e6a276e3aa48ef1bde0c3c9d3ced2c9205ad032f22334f4058e9dfac95a35161",
	"entity:DepreciationSchedule@v2":         "3607f579708ad0c0b57f8e9247c8d769f257f48ea2501b21a92aa9387d7fb735",
	"entity:MaintenanceOrder@v2":             "6ba40ab40280a724de9566f38d5f7c416bcb10eb8ae3b78c46495d987650eecb",
	"entity:Account@v1":                      "21603e07ce6d708bc062f0b21a893e88001d4d004491a6ac291861c2710e4b4f",
	"entity:FiscalYear@v1":                   "4e5ffb474cc2e288efdad6bce811bf137643e2140d5b98597466a94636509210",
	"entity:Period@v1":                       "bc327fb5c8fb3dac7e68faa7e3bbb889cce743fc178cc6a5f8cfc610269af5c5",
	"entity:TaxCode@v2":                      "e071d43f60c3a3c6d16f97dcae26e69507ed54dcd5ebe6e28ada9ed2484a62f2",
	"entity:CostCenter@v1":                   "85a977f57a7e405abbe1b58914a43b801f67c58dc6be445442fa281d878691b0",
	"entity:Item@v2":                         "c2e56680f1d018652d138a8324280fa7ffe70726f5573d9b87241c43bb28119c",
	"entity:PurchaseOrder@v9":                "2cb9382564c9a8f287e730e6bd263eb11091418b88ace0145822f741ef7a3ac9",
	"entity:POLine@v4":                       "b625602cbfd2d13842b86e87de420de79b4652acbea4db5f75ffab973468849c",
	"entity:Facility@v1":                     "a100435dde15704a682190e5c4e700b8de591fa8c41ce923bb34db7bab41d069",
	"entity:InventoryItem@v5":                "5f8ecb2324bc41013687f220e7276cf3b73253b69384c18774cdd8b196c9a7e5",
	"entity:StockTransfer@v2":                "13523ef4edcbc5b73dc341af9aefa8dbe03e62c8389ca324cb2f9e25dc6ca57c",
	"entity:GoodsReceipt@v2":                 "6224376496e7adb1ce68a7abb3705a6201661ae5c38da24cd25a49b0bda6f49d",
	"entity:GoodsReceiptLine@v2":             "9469f6132929fdfdbe06f729bbb14b6002a28b0950ca86442e68febcb93be90d",
	"entity:VendorInvoice@v3":                "00636bf2941e37347611d918d549b7105795efe6b72b0b3cf5c57eacbcc4c9f3",
	"entity:ReorderRule@v2":                  "f513d13eed21e6bca3b0f12a148b1c8a47fa08c3bc883681c96f97c89055139e",
	"entity:RequestForQuotation@v1":          "6f31e507bea1c7f37abaf995ae5942c40f18c06a7927ea06533a11fefe7dea1b",
	"entity:RequestForQuotationLine@v2":      "c773bdc58bc04824cf0ae6a45068aba7c7108a01716e9e55aedba3599e286180",
	"entity:RequestForQuotationVendor@v1":    "9da906b291c92f0d9df38c4b94a7be25d3a3d3a2c0ab036ae4bb47c1a25716dd",
	"entity:RequestForQuotationQuoteLine@v2": "7c912ee86bd1e197c149e77bb1370377fbbd0a9300944f80552eabf39a7918ae",
	"form:Party@v2":                          "c70765af8e5cede38e5e66e2e2b7f1caea95dbfeeaa8f1a6fc43880834b090b7",
	"form:IssueReport@v2":                    "822f3bbed18face521354c33bcd698c195934277780cec7ab742d4d1cd79b442",
	"form:PartyRole@v1":                      "b10a67bcac8866f761320c4a16b6c577ebebd5e1d9fc4e55b3b03a919a186384",
	"form:PartyRelationship@v1":              "4c2ffdb2f4556d7de4bdfac920ee85bdce2fc4db1f7039a04c83506b46d7f948",
	"form:Address@v1":                        "04deefe18e99f1c25e9225dd64622b737b1782ddef1150e87331c4ac7210fb9e",
	"form:ContactMechanism@v1":               "d9ec0a38381ecd9fc974664019f25eea32eb2f580a96112a0c981c1461efb554",
	"form:Attachment@v1":                     "a58827d0f9455a178071ddfa6bd4cf461ff7f809ed4a8bae788dc315208a9571",
	"form:UnitOfMeasure@v1":                  "5518620cd63a4b436dd61ce1b2bcb40068d7f115d75b0e7962a602820e5f7440",
	"form:UomConversion@v1":                  "5fbdc590b156cfcd37db23b5504a895cb577370170d38cfa22586f79d4bb4683",
	"form:Currency@v2":                       "bd2c14fbf8a75cea5de52b802f58a918d6f4892f817fe6b8ce486bd61da3e507",
	"form:ExchangeRate@v1":                   "3afa45bd9091cd5e5277a1e3eec7a103579e6925c003cee58d21c42629322def",
	"form:StatusType@v1":                     "5106b2920f1b814a31f604a12062a47190f595aab6170748da0546959602cddd",
	"form:Status@v1":                         "83b29008764d6b505f50c7db1ebd611f808834ae5a200abfb9e90dce76561de6",
	"form:StatusTransition@v1":               "a919db74029a6b340536fb76ddca478cea506a76f5d4bb70853d361939b367fd",
	"form:Role@v1":                           "c79f12d520c73631f0b561248268880ffcbcf6eebd6b97e35f2a72e2624b2927",
	"form:UserRole@v1":                       "d1a84f5dff9f157880825ea21e46189d4d95fabac1cabdcfadd6b9b5b7a482da",
	"form:Permission@v1":                     "6d635eed4aa35efa2fdb852e253c41d6f9ed57e62a9c2bff167851daf1e22b0c",
	"form:FieldPermission@v1":                "789a9c732ec4ba42f502dadac5e9cd447c3b3ac5e8d1a6536b7c7a1774fab061",
	"form:Department@v1":                     "c832cd8049cc567947db582740e5ef934c9cfd0e53a4c37aea86137cc158e206",
	"form:Position@v1":                       "60b3f9330a5fe9ed0ab8fd0e29b60ae48bc7b2751fead04d026f6a89f7c4ab39",
	"form:Delegation@v1":                     "11a3239fb7880ab260ec6595c6de681e9acddf86ab1eb93c5317b20c52b50c4e",
	"form:SystemOfRecord@v1":                 "f917f70769c5e02a76b8ee3af2bdbdae72d7687a8ae7648a13205effb3a01b7e",
	"form:SalesOrder@v2":                     "d220aa6005b1672216179c35a049418f1acec36852d69aaf23c6455768e81754",
	"form:SOLine@v1":                         "c60736af2fe79691bad7045d666280a1d79a25e299d97f2e7203d2026d536fcd",
	"form:CustomerInvoice@v2":                "381c85a750a240edfc8d3ea8650979e5206d4e37c1881c57a410a960501f81c9",
	"form:Project@v1":                        "8f028dde8328bfee2bd2212c15c9ee83efd807f6af63408ada5b7754df5a0d7c",
	"form:Task@v1":                           "fe622d93cc8a695a4b63ae7ba8b3d26545ebbc0ba65bcc6f58bf9517fdd8bedc",
	"form:TimeEntry@v1":                      "980128ae23584e6a9f5850a6f4a47c7b328b5815a6dec06e1b8696e30a2b9f99",
	"form:ProjectBudgetLine@v1":              "ad6b11161b17bd957d1bbc13c070338b122c3d1763ce4118b15ea241211b1f6c",
	"form:Case@v1":                           "fa342850b4991bdb8b9b1ea993f443ccd6b5fc583f42201fbae22982318af81d",
	"form:Campaign@v1":                       "af301e6308903cf21e60bc4763c1ea44786d3702bed16f787579536181ecf257",
	"form:Lead@v1":                           "01812d3d9c306d6e2799a70b69c2d4693cce001ccbee75bd135fb53b6592df05",
	"form:Opportunity@v1":                    "88fa5e38406dfa70465b3a95fb72025feff9ea82f6a2c8403b204fe670f79544",
	"form:Employee@v2":                       "9fac2093c1fdb687e14065daa81d14e6df70c92380c1aa9b1da979013f4060aa",
	"form:LeaveRequest@v1":                   "eb957392ac0847cdaeab406608c027aa7ae560201bf4154a5cb9170b341206d6",
	"form:AttendanceRecord@v1":               "929caf1314246a03ce67dd6c52e599b64842a72759278740de88e431ea37a7ac",
	"form:FixedAsset@v2":                     "8f12fc975659fa82ca8bc98e36076eac1a193f8e2fa8a7e7fb273c456bc03058",
	"form:DepreciationSchedule@v1":           "79ea708591ecd9323274425ee794e172c48374295526b4bd4dc9a6d53eab0b76",
	"form:MaintenanceOrder@v1":               "7045918cddca54aaac70e4922f611751499d7d37902b771278aaa3f8805149d1",
	"form:Account@v1":                        "dbf9a5e0b9a0b217668d9911d1056abf5846df5c438052f1174264d8ed719f7b",
	"form:FiscalYear@v1":                     "872ffc0dac69960229ec68abf80eb4553d4cffc96c9a448ed6a93a3323646e79",
	"form:Period@v1":                         "1cfa098d2ddc2932ab42d55e15e4b4a46bcc599f5f7e29da04ed6b3ce85b5060",
	"form:TaxCode@v1":                        "5b103f1254e027013b9c4d842d7f9b9fdeef4808604ad7f57ef571e57d8a497f",
	"form:CostCenter@v1":                     "4d6739a53c70d98d39e2719655724ac3a698ecd93e39dace161fba86a4436f99",
	"form:Item@v1":                           "fe6754a587a8114073f00f6f6466e9da97d8a6ee940b3bea6e18cb60fafb7e8e",
	"form:PurchaseOrder@v6":                  "c8cec93b1443889aa71a561b9eb91f08ae1f28ff0481888df432be486be3a431",
	"form:POLine@v1":                         "55b4bf5e9c77e3c74257e5f826b89e92bc6a1da327b809cdb54318c548287079",
	"form:Facility@v1":                       "a00debabc594c43745191249a18fed92f05a49f0d10660331897884e4e9edffa",
	"form:InventoryItem@v2":                  "8e7f407e33c2a26488552b18fa003d189f2cf9c45f61cb6d87aa70bb10a3ce7a",
	"form:StockTransfer@v1":                  "d15f435b1d1e9460c409a6c357aa090ab016a96a64722120ff1d285f48439786",
	"form:GoodsReceipt@v1":                   "5ec7ece55b1962f92c76fb05a75e641e7963f7cd9ab31341afc24465aedfeb2b",
	"form:GoodsReceiptLine@v2":               "42740bd2e28fdf02221efdd77b388ab0ac2bccc2d0d6c9dbce4f7a34d5cdbcb7",
	"form:VendorInvoice@v2":                  "20e7df758babb1952596f96084977baf0f0d0194b6205ec813843f6ddac98449",
	"form:ReorderRule@v1":                    "aa4726b927226d8fe84f1346eabae1c69bc3a79f2eed25bfa4199b7c8b221216",
	"form:RequestForQuotation@v2":            "b017ebbfb8dd539542d4826ddfde8a156b923168fb943f2c9271d52da7e970ae",
	"form:RequestForQuotationLine@v1":        "1c5999eea8566bcfa46fd30d2f893df5f867361388e2490bc1cf248dfd49c0fd",
	"form:RequestForQuotationVendor@v1":      "30f5f7bb15991988d807eab0b3408c52b50502aacee2f7981f96e3a9362a0199",
	"form:RequestForQuotationQuoteLine@v1":   "9756d570d2360c3678f24c6868c9493e294205bdcddd4bfc915cf772c268f6b5",
}

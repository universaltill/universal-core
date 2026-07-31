# Vendored OASIS UBL 2.1 schemas (test-only)

Unmodified copies of the schema files needed to XSD-validate the two
document types this package generates, taken from the **OASIS Universal
Business Language v2.1 OS release (04 November 2013)**:

- Source: https://docs.oasis-open.org/ubl/os-UBL-2.1/xsd/
  (`maindoc/UBL-Order-2.1.xsd`, `maindoc/UBL-Invoice-2.1.xsd`, and the
  `common/` modules they import, including the UN/CEFACT CCTS and W3C
  xmldsig/XAdES signature schemas)
- Each file carries its own upstream copyright header (OASIS Open /
  UN/CEFACT / W3C); redistribution of the unmodified schemas is
  permitted under the OASIS IPR policy noted in those headers.
- To refresh: re-download the same paths from the URL above into
  `maindoc/` and `common/`, preserving the relative layout the
  `schemaLocation` imports expect. Do not edit the files.

Used only by `ubl_test.go`'s xmllint validation (skipped locally when
xmllint is absent; CI installs libxml2-utils so it always runs there).

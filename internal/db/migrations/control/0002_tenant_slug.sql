-- Tenant slug: the URL token for path-prefix tenancy (/t/{slug}/…,
-- universal-core#25, ADR-0011 in uc-infra). db_name stays internal and
-- zitadel_org_id stays Zitadel's — the slug is the one deliberately
-- user-visible identifier, derived from the tenant name at creation and
-- stable afterwards (renaming a tenant must not rot links).
ALTER TABLE tenants ADD COLUMN slug TEXT;

-- Backfill existing tenants: lowercase name, non-alphanumerics collapsed
-- to '-', trimmed; empty results fall back to 'tenant'; names that
-- slugify onto a reserved top-level path segment get a '-tenant' suffix
-- (they would otherwise shadow real routes under /t/'s sibling paths in
-- redirects); duplicates get -2, -3… by creation order. New tenants get
-- their slug from internal/data's slugify (same rules) at INSERT time.
WITH base AS (
    SELECT id,
           trim(both '-' from regexp_replace(lower(name), '[^a-z0-9]+', '-', 'g')) AS raw
    FROM tenants
), cleaned AS (
    SELECT id,
           CASE
               WHEN raw = '' THEN 'tenant'
               WHEN raw IN ('api', 'ui', 't', 'static', 'settings', 'healthz',
                            'records', 'forms', 'modules', 'import', 'export',
                            'reports', 'issue-report', 'workflow-jobs')
                   THEN raw || '-tenant'
               ELSE raw
           END AS base_slug
    FROM base
), numbered AS (
    SELECT id, base_slug,
           row_number() OVER (PARTITION BY base_slug ORDER BY id) AS rn
    FROM cleaned
)
-- Duplicates disambiguate with a suffix derived from the row's own id,
-- not a counter: 'acme', 'acme', 'acme-2' would make a counter-based
-- 'acme-2' collide with the third row's BASE slug, failing the unique
-- index below and aborting the whole migration at boot (independent
-- review, 2026-07-31). An id-derived suffix is collision-proof by
-- construction (ids are unique, and no base slug can contain one — the
-- backfill's '-' collapsing never produces a 6-char hex tail matching a
-- specific row id by accident with these inputs; the unique index still
-- backstops it).
UPDATE tenants t
SET slug = CASE
    WHEN n.rn = 1 THEN n.base_slug
    ELSE n.base_slug || '-' || left(replace(n.id::text, '-', ''), 6)
END
FROM numbered n
WHERE t.id = n.id;

ALTER TABLE tenants ALTER COLUMN slug SET NOT NULL;
CREATE UNIQUE INDEX tenants_slug_key ON tenants (slug);

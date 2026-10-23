// WORK-RESOURCES-001 / BND-003: validation helpers for the web resource
// deep links (/platform/toolbox/skills?catalog_id=..., /platform/
// knowledge-bases/:kbId?knowledge_id=...) and the post-login return target.
// These functions are deliberately pure so the security-relevant edge cases —
// hostile return URLs, dot-dot ids, control characters — are unit-testable
// without mounting the SPA.

export interface SkillDeepLinkTarget {
  catalogId: string
  tenantId: number | null
}

const MAX_RESOURCE_ID_LENGTH = 128

function hasControlChars(value: string): boolean {
  for (let i = 0; i < value.length; i++) {
    const code = value.charCodeAt(i)
    if (code < 0x21 || code === 0x7f) return true
  }
  return false
}

/** A single resource id: non-empty, bounded, no separators, no dot-dot (DATA-101). */
export function isSafeResourceId(value: unknown): value is string {
  if (typeof value !== 'string') return false
  const trimmed = value.trim()
  if (trimmed === '' || trimmed !== value) return false
  if (value.length > MAX_RESOURCE_ID_LENGTH) return false
  if (value.includes('..')) return false
  if (value.includes('/') || value.includes('\\')) return false
  if (value.includes('%')) return false
  return !hasControlChars(value)
}

/** tenant_id query values must be plain integers with no padding. */
export function parseTenantIdParam(value: unknown): number | null {
  if (typeof value !== 'string') return null
  if (!/^\d{1,19}$/.test(value)) return null
  const parsed = Number(value)
  if (!Number.isSafeInteger(parsed)) return null
  return parsed
}

/**
 * Parse the skill detail deep link. A malformed catalog_id makes the whole
 * target null (the caller then keeps the plain skills page instead of
 * guessing); tenant_id is optional and only ever used after a membership
 * check — never as an authorization grant.
 */
export function parseSkillDeepLink(query: Record<string, unknown>): SkillDeepLinkTarget | null {
  const rawId = query.catalog_id
  if (rawId === undefined || rawId === null || rawId === '') {
    return null
  }
  const catalogId = Array.isArray(rawId) ? rawId[0] : rawId
  if (!isSafeResourceId(catalogId)) return null
  const rawTenant = Array.isArray(query.tenant_id) ? query.tenant_id[0] : query.tenant_id
  return { catalogId, tenantId: parseTenantIdParam(rawTenant) }
}

/**
 * A login redirect target is honored only when it is a same-origin relative
 * path: it must start with '/', must not start with '//' (protocol-relative
 * external URLs), must not smuggle a scheme ('/\\', control characters) and
 * must stay inside the platform shell. Anything else returns null and the
 * login flow falls back to the default landing page.
 */
export function safeLoginRedirect(value: unknown): string | null {
  if (typeof value !== 'string') return null
  if (value === '' || value.length > 2048) return null
  if (!value.startsWith('/') || value.startsWith('//')) return null
  if (value.includes('://') || value.includes('\\')) return null
  if (hasControlChars(value)) return null
  if (!value.startsWith('/platform/') && value !== '/platform') return null
  return value
}

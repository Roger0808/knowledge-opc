import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

// Source-level contract tests (same style as settingsRoute.test.mjs): the
// login bounce preserves a validated same-origin platform target, and the
// login page only honors targets that pass safeLoginRedirect.

const router = readFileSync(new URL('./index.ts', import.meta.url), 'utf8')
const login = readFileSync(new URL('../views/auth/Login.vue', import.meta.url), 'utf8')
const skillSettings = readFileSync(new URL('../views/settings/SkillSettings.vue', import.meta.url), 'utf8')

test('unauthenticated bounces to /login keep a safe redirect target', () => {
  assert.match(router, /import \{ safeLoginRedirect \} from '@\/utils\/resourceDeepLink'/)
  assert.match(router, /function loginRedirectLocation\(to: RouteLocationNormalized\)/)
  assert.match(router, /safeLoginRedirect\(to\.fullPath\)/)
  assert.match(router, /\{ path: '\/login', query: \{ redirect: target \} \}/)
  // Both auth gates (onboarding + main) bounce through the same helper.
  assert.equal((router.match(/next\(loginRedirectLocation\(to\)\)/g) || []).length, 2)
  // The raw '/login' string must not survive as a next() target on the
  // guarded paths anymore.
  assert.doesNotMatch(router, /next\('\/login'\)/)
})

test('login success returns to the redirect target only after validation', () => {
  assert.match(login, /import \{ safeLoginRedirect \} from '@\/utils\/resourceDeepLink'/)
  assert.match(login, /safeLoginRedirect\(route\.query\.redirect\)/)
  // The honored target requires a valid tenant; otherwise the default
  // landing page applies.
  assert.match(login, /if \(redirectTarget && authStore\.hasValidTenant\)/)
})

test('skill deep link resolves by exact catalog id and never substitutes', () => {
  assert.match(skillSettings, /import \{ parseSkillDeepLink \} from '@\/utils\/resourceDeepLink'/)
  assert.match(skillSettings, /resolveSkillDeepLink\(\)/)
  // Exact-id lookup; the not-found branch reports an error instead of
  // falling back to a same-named skill (VAL-204).
  assert.match(skillSettings, /catalog\.value\.find\(\(item\) => item\.id === target\.catalogId\)/)
  assert.match(skillSettings, /settings\.skills\.deepLinkNotFound/)
  // tenant_id is applied only after a membership check.
  assert.match(skillSettings, /authStore\.memberships\.find\(\(m\) => m\.tenant_id === target\.tenantId\)/)
  assert.match(skillSettings, /settings\.skills\.deepLinkTenantForbidden/)
})

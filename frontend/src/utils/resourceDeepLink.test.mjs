import assert from 'node:assert/strict'
import test from 'node:test'

import {
  isSafeResourceId,
  parseSkillDeepLink,
  parseTenantIdParam,
  safeLoginRedirect,
} from './resourceDeepLink.ts'

test('isSafeResourceId accepts ordinary catalog ids', () => {
  assert.equal(isSafeResourceId('cat-1'), true)
  assert.equal(isSafeResourceId('9f2c'), true)
  assert.equal(isSafeResourceId('a'.repeat(128)), true)
})

test('isSafeResourceId rejects traversal, separators and control chars', () => {
  assert.equal(isSafeResourceId(''), false)
  assert.equal(isSafeResourceId('  '), false)
  assert.equal(isSafeResourceId('a b'), false)
  assert.equal(isSafeResourceId('..'), false)
  assert.equal(isSafeResourceId('a..b'), false)
  assert.equal(isSafeResourceId('a/b'), false)
  assert.equal(isSafeResourceId('a\\b'), false)
  assert.equal(isSafeResourceId('a%2fb'), false)
  assert.equal(isSafeResourceId('a\u0000b'), false)
  assert.equal(isSafeResourceId('a\u007fb'), false)
  assert.equal(isSafeResourceId(' padded'), false)
  assert.equal(isSafeResourceId('a'.repeat(129)), false)
  assert.equal(isSafeResourceId(42), false)
  assert.equal(isSafeResourceId(undefined), false)
})

test('parseTenantIdParam only accepts plain integers', () => {
  assert.equal(parseTenantIdParam('1001'), 1001)
  assert.equal(parseTenantIdParam(' 1001 '), null)
  assert.equal(parseTenantIdParam('10x1'), null)
  assert.equal(parseTenantIdParam('-5'), null)
  assert.equal(parseTenantIdParam('1e5'), null)
  assert.equal(parseTenantIdParam('99999999999999999999'), null)
  assert.equal(parseTenantIdParam(undefined), null)
})

test('parseSkillDeepLink reads catalog_id and tenant_id', () => {
  assert.deepEqual(parseSkillDeepLink({ catalog_id: 'cat-9' }), { catalogId: 'cat-9', tenantId: null })
  assert.deepEqual(parseSkillDeepLink({ catalog_id: 'cat-9', tenant_id: '1002' }), {
    catalogId: 'cat-9',
    tenantId: 1002,
  })
  // No catalog_id → keep plain behavior, never an error state.
  assert.equal(parseSkillDeepLink({}), null)
  assert.equal(parseSkillDeepLink({ tenant_id: '1002' }), null)
  // Hostile or malformed catalog_id must not resolve to a guessable target.
  assert.equal(parseSkillDeepLink({ catalog_id: '../etc' }), null)
  assert.equal(parseSkillDeepLink({ catalog_id: 'a/b' }), null)
  assert.equal(parseSkillDeepLink({ catalog_id: 'x'.repeat(200) }), null)
  assert.equal(parseSkillDeepLink({ catalog_id: ['../etc', 'cat-2'] }), null)
  // Repeated params resolve deterministically to the first value, and only
  // when every occurrence is safe.
  assert.deepEqual(parseSkillDeepLink({ catalog_id: ['cat-1', 'cat-2'] }), {
    catalogId: 'cat-1',
    tenantId: null,
  })
  // Array first element is tolerated the way URLSearchParams repeats are.
  assert.deepEqual(parseSkillDeepLink({ catalog_id: ['cat-7', 'cat-8'], tenant_id: ['42'] }), {
    catalogId: 'cat-7',
    tenantId: 42,
  })
  // A malformed tenant_id never blocks the catalog target.
  assert.deepEqual(parseSkillDeepLink({ catalog_id: 'cat-9', tenant_id: 'abc' }), {
    catalogId: 'cat-9',
    tenantId: null,
  })
})

test('safeLoginRedirect honors same-origin platform targets', () => {
  assert.equal(safeLoginRedirect('/platform/toolbox/skills?catalog_id=cat-1'),
    '/platform/toolbox/skills?catalog_id=cat-1')
  assert.equal(safeLoginRedirect('/platform/knowledge-bases/kb-1?knowledge_id=d-1'),
    '/platform/knowledge-bases/kb-1?knowledge_id=d-1')
  assert.equal(safeLoginRedirect('/platform'), '/platform')
})

test('safeLoginRedirect rejects external and hostile targets', () => {
  assert.equal(safeLoginRedirect('https://evil.example/x'), null)
  assert.equal(safeLoginRedirect('//evil.example/x'), null)
  assert.equal(safeLoginRedirect('///evil'), null)
  assert.equal(safeLoginRedirect('/\\evil.example/x'), null)
  assert.equal(safeLoginRedirect('/platform\u0000.evil'), null)
  assert.equal(safeLoginRedirect('/login'), null)
  assert.equal(safeLoginRedirect('/outside'), null)
  assert.equal(safeLoginRedirect(''), null)
  assert.equal(safeLoginRedirect(null), null)
  assert.equal(safeLoginRedirect('/platform/' + 'a'.repeat(2100)), null)
  assert.equal(safeLoginRedirect('javascript:alert(1)'), null)
})

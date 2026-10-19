import assert from 'node:assert/strict'
import { test } from 'node:test'
import { readFile } from 'node:fs/promises'
import ts from 'typescript'

const source = await readFile(new URL('../src/lib/model-status.ts', import.meta.url), 'utf8')
const { outputText } = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2020, module: ts.ModuleKind.ESNext } })
const {
  selectedModelStatuses, mergeSelectableModels, filterModelGroup, modelGroupCounts,
  modelStatusCounts, filterModelStatus, sortModelStatuses, saveModelSelection, readSelectionConfig,
} = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`)

const status = (name, requests = 1, rate = 100) => ({
  model_name: name, display_name: name, time_window: '24h', total_requests: requests,
  success_count: requests, success_rate: rate, current_status: rate >= 95 ? 'green' : 'yellow', slot_data: [],
})
const json = (data, httpStatus = 200) => new Response(JSON.stringify(data), { status: httpStatus })

test('12 selected models remain exactly 12, including zero-traffic and missing data; old 47-model response is trimmed', () => {
  const selected = Array.from({ length: 12 }, (_, index) => `selected-${index}`)
  const old = [...selected.slice(0, 11).map(name => status(name, 0)), ...Array.from({ length: 36 }, (_, index) => status(`old-${index}`))]
  assert.equal(old.length, 47)
  const result = selectedModelStatuses(selected, old, '24h')
  assert.deepEqual(result.map(model => model.model_name), selected)
  assert.equal(modelStatusCounts(result).empty, 12)
  assert.equal(modelStatusCounts(result).green, 0)
  assert.equal(result.at(-1).slot_data.length, 24)
})

test('a response for a previous time window is not displayed as current data', () => {
  const result = selectedModelStatuses(['a'], [status('a', 20)], '1h')
  assert.equal(result[0].total_requests, 0)
  assert.equal(result[0].time_window, '1h')
  assert.equal(result[0].slot_data.length, 60)
})

test('group filters intersect the selected list and never expand it', () => {
  const models = selectedModelStatuses(['a', 'b'], [status('a'), status('b', 0)], '24h')
  const tokens = [{ group_name: 'svip', models: ['b', 'not-selected'] }]
  const groups = [{ id: 'custom', models: ['a'] }]
  assert.deepEqual(filterModelGroup(models, 'token:svip', groups, tokens).map(model => model.model_name), ['b'])
  assert.deepEqual(modelGroupCounts(models, groups, tokens), { all: 2, custom: 1, 'token:svip': 1 })
})

test('the selector exposes saved historical models missing from 24h candidates', () => {
  assert.deepEqual(mergeSelectableModels([{ model_name: 'new', request_count_24h: 3 }], ['historical', 'new']), [
    { model_name: 'new', request_count_24h: 3 }, { model_name: 'historical', request_count_24h: 0 },
  ])
})

test('zero-request green defaults are no-data, not healthy; filters and counts agree', () => {
  const models = [status('healthy'), status('warning', 3, 90), status('no-data', 0)]
  assert.deepEqual(modelStatusCounts(models), { green: 1, yellow: 1, red: 0, empty: 1 })
  assert.deepEqual(filterModelStatus(models, 'green').map(model => model.model_name), ['healthy'])
  assert.deepEqual(filterModelStatus(models, 'empty').map(model => model.model_name), ['no-data'])
})

test('both views can use the same availability/custom/default ordering', () => {
  const models = [status('no-data', 0), status('warning', 1, 90), status('healthy')]
  assert.deepEqual(sortModelStatuses(models, 'availability', []).map(model => model.model_name), ['healthy', 'warning', 'no-data'])
  assert.deepEqual(sortModelStatuses(models, 'custom', ['warning', 'healthy']).map(model => model.model_name), ['warning', 'healthy', 'no-data'])
  assert.deepEqual(sortModelStatuses(models, 'default', []).map(model => model.model_name), ['no-data', 'warning', 'healthy'])
})

test('no-data ordering does not depend on a backend default success rate', () => {
  const models = [status('missing', 0, 0), status('default', 0, 100)]
  assert.deepEqual(sortModelStatuses(models, 'availability', []).map(model => model.model_name), ['missing', 'default'])
})

test('empty configuration is authoritative, not a request to auto-select recent models', async () => {
  const config = await readSelectionConfig('/selected', {}, async () => json({ success: true, data: [] }))
  assert.deepEqual(config.data, [])
  assert.deepEqual(selectedModelStatuses(config.data, [status('old')], '24h'), [])
})

test('failed HTTP and false success saves are rejected without pretending to commit', async () => {
  await assert.rejects(saveModelSelection('/selected', ['a'], {}, async () => json({ success: false }, 401)), /保存模型名单失败/)
  await assert.rejects(saveModelSelection('/selected', ['a'], {}, async () => json({ success: false })), /保存模型名单失败/)
  await assert.rejects(saveModelSelection('/selected', ['a'], {}, async () => json(null)), /保存模型名单失败/)
})

test('save requires fresh server readback and rejects a stale 47-model selection', async () => {
  const calls = []
  const selected = Array.from({ length: 12 }, (_, index) => `selected-${index}`)
  const previous = Array.from({ length: 47 }, (_, index) => `old-${index}`)
  const request = async (url, init) => {
    calls.push({ url, init })
    return init.method === 'POST' ? json({ success: true }) : json({ success: true, data: previous })
  }
  await assert.rejects(saveModelSelection('/selected', selected, {}, request), /回读不一致/)
  assert.equal(calls[1].init.cache, 'no-store')
  assert.deepEqual(JSON.parse(calls[0].init.body), { models: selected })
})

test('successful empty/nonempty saves return only the verified server selection', async () => {
  for (const selected of [[], ['a', 'b']]) {
    const request = async (_url, init) => json({ success: true, data: init.method === 'POST' ? selected : selected })
    assert.deepEqual(await saveModelSelection('/selected', selected, {}, request), selected)
  }
})

test('failed config loads are not converted to an empty, auto-saved configuration', async () => {
  await assert.rejects(readSelectionConfig('/selected', {}, async () => json({ success: false }, 503)), /读取模型名单失败/)
  await assert.rejects(readSelectionConfig('/selected', {}, async () => json({ success: true, data: null })), /响应无效/)
  await assert.rejects(readSelectionConfig('/selected', {}, async () => json(null)), /响应无效/)
})

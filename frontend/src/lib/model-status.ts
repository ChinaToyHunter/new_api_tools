export interface SlotStatus {
  slot: number
  start_time: number
  end_time: number
  total_requests: number
  success_count: number
  success_rate: number
  status: 'green' | 'yellow' | 'red'
}

export interface ModelStatus {
  model_name: string
  display_name: string
  time_window: string
  total_requests: number
  success_count: number
  success_rate: number
  current_status: 'green' | 'yellow' | 'red'
  slot_data: SlotStatus[]
}

export type SortMode = 'default' | 'availability' | 'custom'
export type StatusFilter = 'all' | 'green' | 'yellow' | 'red' | 'empty'

export interface ModelGroup {
  id: string
  models: string[]
  icon?: string
}

export interface TokenGroup {
  group_name: string
  models: string[]
}

const VENDOR_KEYWORDS: Record<string, string[]> = {
  openai: ['gpt', 'openai', 'o1', 'o3', 'chatgpt', 'dall-e', 'whisper', 'tts'],
  claude: ['claude', 'anthropic'],
  gemini: ['gemini', 'gemma', 'bard'],
  deepseek: ['deepseek'],
  meta: ['llama', 'meta'],
  mistral: ['mistral', 'mixtral', 'codestral', 'pixtral'],
  qwen: ['qwen', 'tongyi'],
  zhipu: ['glm', 'chatglm', 'zhipu'],
  moonshot: ['moonshot', 'kimi'],
  kimi: ['kimi', 'moonshot'],
  doubao: ['doubao', 'bytedance'],
  minimax: ['minimax', 'abab'],
  baichuan: ['baichuan'],
  yi: ['yi-', '01-ai', 'zero-one'],
  spark: ['spark', 'xunfei'],
  hunyuan: ['hunyuan', 'tencent'],
  stepfun: ['stepfun', 'step-'],
  wenxin: ['wenxin', 'ernie', 'baidu'],
  cohere: ['cohere', 'command'],
  perplexity: ['perplexity', 'pplx', 'sonar'],
  groq: ['groq'],
  ollama: ['ollama'],
  together: ['together'],
  openrouter: ['openrouter'],
  siliconcloud: ['siliconcloud', 'silicon'],
  coze: ['coze'],
  cerebras: ['cerebras'],
}

export function modelMatchesGroup(modelName: string, group: ModelGroup): boolean {
  if (group.models.includes(modelName)) return true
  const keywords = group.icon ? VENDOR_KEYWORDS[group.icon] : undefined
  return keywords?.some(keyword => modelName.toLowerCase().includes(keyword)) ?? false
}

export function normalizeModels(models: string[]): string[] {
  return [...new Set(models.map(model => model.trim()).filter(Boolean))]
}

export function sameModels(a: string[], b: string[]): boolean {
  return a.length === b.length && a.every((model, index) => model === b[index])
}

export function mergeSelectableModels(
  available: { model_name: string; request_count_24h: number }[],
  selected: string[],
) {
  const candidates = new Map(available.map(model => [model.model_name, model]))
  selected.forEach(model => {
    if (!candidates.has(model)) candidates.set(model, { model_name: model, request_count_24h: 0 })
  })
  return [...candidates.values()]
}

// The saved selection, not recent traffic or token-group membership, defines the cards.
export function selectedModelStatuses(selected: string[], statuses: ModelStatus[], timeWindow: string): ModelStatus[] {
  const byName = new Map(statuses.filter(model => model.time_window === timeWindow).map(model => [model.model_name, model]))
  const hours = ({ '1h': 1, '6h': 6, '12h': 12, '24h': 24 } as Record<string, number>)[timeWindow] || 24
  const slots = timeWindow === '1h' ? 60 : 24
  const end = Math.floor(Date.now() / 1000)
  const slotSeconds = hours * 3600 / slots
  return normalizeModels(selected).map(name => byName.get(name) ?? {
    model_name: name,
    display_name: name,
    time_window: timeWindow,
    total_requests: 0,
    success_count: 0,
    success_rate: 0,
    current_status: 'green',
    slot_data: Array.from({ length: slots }, (_, index) => ({
      slot: index,
      start_time: end - (slots - index) * slotSeconds,
      end_time: end - (slots - index - 1) * slotSeconds,
      total_requests: 0,
      success_count: 0,
      success_rate: 0,
      status: 'green',
    })),
  })
}

export function filterModelGroup<T extends { model_name: string }>(
  models: T[], filter: string, groups: ModelGroup[], tokenGroups: TokenGroup[],
): T[] {
  if (filter === 'all') return models
  if (filter.startsWith('token:')) {
    const group = tokenGroups.find(group => group.group_name === filter.slice(6))
    return group ? models.filter(model => group.models.includes(model.model_name)) : models
  }
  const group = groups.find(group => group.id === filter)
  return group ? models.filter(model => modelMatchesGroup(model.model_name, group)) : models
}

export function modelGroupCounts(models: ModelStatus[], groups: ModelGroup[], tokenGroups: TokenGroup[]) {
  const counts: Record<string, number> = { all: models.length }
  groups.forEach(group => { counts[group.id] = filterModelGroup(models, group.id, groups, tokenGroups).length })
  tokenGroups.forEach(group => {
    counts[`token:${group.group_name}`] = filterModelGroup(models, `token:${group.group_name}`, groups, tokenGroups).length
  })
  return counts
}

export function modelStatusCounts(models: ModelStatus[]) {
  const counts = { green: 0, yellow: 0, red: 0, empty: 0 }
  models.forEach(model => { counts[model.total_requests > 0 ? model.current_status : 'empty']++ })
  return counts
}

export function filterModelStatus(models: ModelStatus[], filter: StatusFilter) {
  if (filter === 'all') return models
  return models.filter(model => filter === 'empty' ? model.total_requests === 0 : model.total_requests > 0 && model.current_status === filter)
}

export function sortModelStatuses(models: ModelStatus[], mode: SortMode, customOrder: string[]) {
  if (mode === 'availability') {
    return [...models].sort((a, b) => Number(b.total_requests > 0) - Number(a.total_requests > 0) || (a.total_requests > 0 ? b.success_rate - a.success_rate : 0))
  }
  if (mode === 'custom') {
    const rank = new Map(customOrder.map((name, index) => [name, index]))
    return [...models].sort((a, b) => (rank.get(a.model_name) ?? Infinity) - (rank.get(b.model_name) ?? Infinity))
  }
  return models
}

interface SelectionConfig {
  success: boolean
  data: string[]
  time_window?: string
  theme?: string
  refresh_interval?: number
  sort_mode?: SortMode
  custom_order?: string[] | null
  custom_groups?: (ModelGroup & { name: string })[]
  site_title?: string
}

export async function readSelectionConfig(url: string, init: RequestInit = {}, request = fetch): Promise<SelectionConfig> {
  const response = await request(url, { ...init, cache: 'no-store' })
  if (!response.ok) throw new Error('读取模型名单失败')
  const config = await response.json()
  if (!config?.success || !Array.isArray(config.data) || !config.data.every((model: unknown) => typeof model === 'string')) {
    throw new Error('模型名单响应无效')
  }
  return { ...config, data: normalizeModels(config.data) }
}

export async function saveModelSelection(url: string, models: string[], headers: HeadersInit, request = fetch): Promise<string[]> {
  const selected = normalizeModels(models)
  const response = await request(url, { method: 'POST', headers, body: JSON.stringify({ models: selected }) })
  if (!response.ok) throw new Error('保存模型名单失败')
  const result = await response.json()
  if (!result?.success) throw new Error('保存模型名单失败')
  const config = await readSelectionConfig(url, { headers }, request)
  if (!sameModels(selected, config.data)) throw new Error('模型名单回读不一致，请重新保存')
  return config.data
}

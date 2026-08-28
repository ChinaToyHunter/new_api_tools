import { useState, useEffect, useCallback, useRef } from 'react'
import { useToast } from './Toast'
import { useAuth } from '../contexts/AuthContext'
import {
  Settings as SettingsIcon, Loader2, RefreshCw, CheckCircle2, XCircle,
  Rocket, DownloadCloud, AlertTriangle, Info, ExternalLink,
} from 'lucide-react'
import { Card, CardContent, CardHeader, CardTitle, CardDescription } from './ui/card'
import { Button } from './ui/button'
import { Badge } from './ui/badge'

interface UpdateStatus {
  configured: boolean
  container_name: string
  current_version: string
  version_is_dev: boolean
  current_image_ref: string
  remote_commit: string
  update_available: boolean
  release_url: string
}

interface CheckResult {
  container: string
  current_version: string
  update_available: boolean
  image?: string
  latest_digest?: string
  watchtower_count?: number
}

type UpdatePhase = 'idle' | 'running' | 'restarting' | 'done' | 'failed'

const POLL_INTERVAL_MS = 5000
const RESTART_TIMEOUT_MS = 8 * 60 * 1000 // 与 watchtower 5m timeout + 拉镜像余量对齐

export function Settings() {
  const { showToast } = useToast()
  const { token } = useAuth()

  const [status, setStatus] = useState<UpdateStatus | null>(null)
  const [checkResult, setCheckResult] = useState<CheckResult | null>(null)
  const [loading, setLoading] = useState(true)
  const [checking, setChecking] = useState(false)
  const [phase, setPhase] = useState<UpdatePhase>('idle')
  const [phaseMessage, setPhaseMessage] = useState('')
  const healthPollTimerRef = useRef<ReturnType<typeof setInterval> | null>(null)

  const apiUrl = import.meta.env.VITE_API_URL || ''
  const getAuthHeaders = useCallback(() => ({
    'Content-Type': 'application/json',
    'Authorization': `Bearer ${token}`,
  }), [token])

  const setPhaseSafe = (p: UpdatePhase) => {
    setPhase(p)
  }

  const stopHealthPoll = useCallback(() => {
    if (healthPollTimerRef.current !== null) {
      clearInterval(healthPollTimerRef.current)
      healthPollTimerRef.current = null
    }
  }, [])

  const fetchStatus = useCallback(async (silent = false) => {
    if (!silent) setLoading(true)
    try {
      const response = await fetch(`${apiUrl}/api/system/update/status`, { headers: getAuthHeaders() })
      const data = await response.json()
      if (data.success) {
        setStatus(data.data)
      } else {
        showToast('error', data.message || '获取更新状态失败')
      }
    } catch (error) {
      console.error('Failed to fetch update status:', error)
      showToast('error', '网络错误，请重试')
    } finally { if (!silent) setLoading(false) }
  }, [apiUrl, getAuthHeaders, showToast])

  useEffect(() => { fetchStatus() }, [fetchStatus])
  useEffect(() => stopHealthPoll, [stopHealthPoll])

  const handleCheck = async () => {
    setChecking(true)
    setCheckResult(null)
    try {
      const response = await fetch(`${apiUrl}/api/system/update/check`, {
        method: 'POST',
        headers: getAuthHeaders(),
      })
      const data = await response.json()
      if (data.success) {
        setCheckResult(data.data)
        if (data.data.update_available) {
          showToast('info', '检测到新版本镜像')
        } else {
          showToast('success', '当前已是最新镜像')
        }
      } else {
        showToast('error', data.error?.message || data.message || '检查更新失败')
      }
    } catch (error) {
      showToast('error', '网络错误，请重试')
      console.error('Failed to check update:', error)
    } finally { setChecking(false) }
  }

  // 一键更新：触发 async 更新 → 轮询未鉴权 /api/health 等容器重启回来 → 刷新版本
  const handleRunUpdate = async () => {
    if (!window.confirm('确定要立即更新吗？更新过程中服务会短暂中断（容器将被拉取新镜像并重建）。')) return

    setPhaseSafe('running')
    setPhaseMessage('正在触发更新...')
    try {
      const response = await fetch(`${apiUrl}/api/system/update/run`, {
        method: 'POST',
        headers: getAuthHeaders(),
      })
      const data = await response.json()
      if (!data.success) {
        if (data.error?.code === 'NO_UPDATE_AVAILABLE') {
          setCheckResult(current => current ? { ...current, update_available: false } : current)
        }
        setPhaseSafe('failed')
        setPhaseMessage(data.error?.message || data.message || '触发更新失败')
        showToast('error', data.error?.message || data.message || '触发更新失败')
        return
      }

      setCheckResult(null)
      setPhaseSafe('restarting')
      setPhaseMessage('更新已触发，容器正在拉取新镜像并重启，页面将自动恢复...')
      pollHealth(status?.current_version || '', data.data?.status_code)
    } catch (error) {
      setPhaseSafe('failed')
      setPhaseMessage('网络错误：请求可能已发出，请稍后刷新页面确认')
      console.error('Failed to run update:', error)
    }
  }

  const pollHealth = (previousVersion: string, triggerStatusCode?: number) => {
    stopHealthPoll()
    const startedAt = Date.now()
    let sawDown = false
    let pollInFlight = false

    healthPollTimerRef.current = setInterval(async () => {
      if (pollInFlight) return
      pollInFlight = true

      // async=202 只表示任务已接收，不代表容器已经重启。只有观察到掉线后恢复，
      // 或健康接口报告了不同构建版本，才宣告更新完成。
      try {
        const response = await fetch(`${apiUrl}/api/health`, { cache: 'no-store' })
        if (response.ok) {
          const health = await response.json().catch(() => null)
          const reportedVersion = typeof health?.version === 'string' ? health.version : ''
          const versionChanged = previousVersion !== '' && reportedVersion !== '' && reportedVersion !== previousVersion

          if (sawDown || versionChanged) {
            stopHealthPoll()
            setPhaseSafe('done')
            setPhaseMessage(`更新完成，容器已恢复${reportedVersion ? `（版本 ${reportedVersion.slice(0, 12)}）` : ''}`)
            showToast('success', '更新完成')
            await fetchStatus(true)
            return
          }

          setPhaseMessage(triggerStatusCode === 202
            ? '更新任务执行中，正在等待容器重启...'
            : '服务仍在线，正在等待版本变化...')
        } else {
          sawDown = true
          setPhaseMessage('已检测到服务重启，正在等待新容器恢复...')
        }
      } catch {
        sawDown = true
        setPhaseMessage('已检测到服务重启，正在等待新容器恢复...')
      } finally {
        pollInFlight = false
      }

      if (Date.now() - startedAt > RESTART_TIMEOUT_MS) {
        stopHealthPoll()
        setPhaseSafe('failed')
        setPhaseMessage('等待容器恢复超时，请到服务器查看 watchtower 日志（docker logs newapi-tools-watchtower）')
        showToast('error', '等待容器恢复超时')
      }
    }, POLL_INTERVAL_MS)
  }

  const versionLabel = status ? (status.version_is_dev ? 'dev（本地构建）' : status.current_version) : '—'

  return (
    <div className="space-y-6">
      <div className="flex items-center gap-3">
        <div className="p-2 rounded-lg bg-primary/10">
          <SettingsIcon className="h-5 w-5 text-primary" />
        </div>
        <div>
          <h1 className="text-2xl font-bold">设置</h1>
          <p className="text-sm text-muted-foreground">系统版本与一键更新</p>
        </div>
      </div>

      {loading ? (
        <div className="flex items-center justify-center py-12">
          <Loader2 className="h-6 w-6 animate-spin text-muted-foreground" />
        </div>
      ) : (
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              版本与更新
              {status?.update_available && phase !== 'running' && phase !== 'restarting' && (
                <Badge variant="warning">远端有新提交</Badge>
              )}
            </CardTitle>
            <CardDescription>
              通过 Watchtower sidecar 拉取最新镜像并重建容器；更新期间服务短暂中断
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            <div className="grid gap-3 sm:grid-cols-2">
              <div className="rounded-lg border bg-muted/30 p-3">
                <div className="text-xs text-muted-foreground mb-1">当前版本（构建 commit）</div>
                <div className="font-mono text-sm">{versionLabel}</div>
              </div>
              <div className="rounded-lg border bg-muted/30 p-3">
                <div className="text-xs text-muted-foreground mb-1">远端最新 commit</div>
                <div className="font-mono text-sm break-all">
                  {status?.remote_commit
                    ? status.remote_commit.slice(0, 12)
                    : (status?.version_is_dev ? '—（dev 构建不比对）' : '获取失败或未配置')}
                </div>
              </div>
              <div className="rounded-lg border bg-muted/30 p-3 sm:col-span-2">
                <div className="text-xs text-muted-foreground mb-1">目标容器 / 镜像</div>
                <div className="font-mono text-sm break-all">
                  {status?.container_name || '—'}
                  <span className="text-muted-foreground"> ← </span>
                  {status?.current_image_ref || '—'}
                </div>
              </div>
            </div>

            {checkResult && (
              <div className="flex items-center gap-2 text-sm rounded-lg border p-3 bg-background">
                {checkResult.update_available ? (
                  <><DownloadCloud className="h-4 w-4 text-amber-500" />
                    <span>Watchtower 检测到新镜像 digest，可执行一键更新</span></>
                ) : (
                  <><CheckCircle2 className="h-4 w-4 text-emerald-500" />
                    <span>Watchtower 确认镜像已是最新</span></>
                )}
              </div>
            )}

            {phase !== 'idle' && (
              <div
                className={`flex items-start gap-2 text-sm rounded-lg border p-3 ${
                  phase === 'failed'
                    ? 'border-destructive/40 bg-destructive/10'
                    : phase === 'done'
                      ? 'border-emerald-500/40 bg-emerald-500/10'
                      : 'border-primary/40 bg-primary/10'
                }`}
              >
                {phase === 'failed' ? (
                  <XCircle className="h-4 w-4 mt-0.5 text-destructive shrink-0" />
                ) : phase === 'done' ? (
                  <CheckCircle2 className="h-4 w-4 mt-0.5 text-emerald-500 shrink-0" />
                ) : (
                  <Loader2 className="h-4 w-4 mt-0.5 animate-spin shrink-0" />
                )}
                <span>{phaseMessage}</span>
              </div>
            )}

            {status?.configured ? (
              <div className="flex flex-wrap gap-2">
                <Button variant="outline" onClick={handleCheck} disabled={checking || phase === 'running' || phase === 'restarting'}>
                  {checking ? <Loader2 className="h-4 w-4 mr-2 animate-spin" /> : <RefreshCw className="h-4 w-4 mr-2" />}
                  检查更新
                </Button>
                <Button
                  onClick={handleRunUpdate}
                  disabled={!checkResult?.update_available || phase === 'running' || phase === 'restarting'}
                  title={checkResult?.update_available ? '更新到检查出的最新镜像' : '请先检查并确认有可用镜像'}
                >
                  {(phase === 'running' || phase === 'restarting')
                    ? <Loader2 className="h-4 w-4 mr-2 animate-spin" />
                    : <Rocket className="h-4 w-4 mr-2" />}
                  {phase === 'restarting' ? '更新中...' : '一键更新'}
                </Button>
              </div>
            ) : (
              <NotConfiguredNotice status={status} />
            )}

            <div className="flex items-center gap-2 text-xs text-muted-foreground">
              <Info className="h-3.5 w-3.5" />
              <span>更新不可自动回滚：如需回滚，在服务器上用上一个 commit 的镜像 tag 重新 up。
                <a
                  className="inline-flex items-center gap-0.5 text-primary hover:underline ml-1"
                  href={status?.release_url || 'https://github.com/ChinaToyHunter/new_api_tools'}
                  target="_blank"
                  rel="noreferrer"
                >
                  仓库 <ExternalLink className="h-3 w-3" />
                </a>
              </span>
            </div>
          </CardContent>
        </Card>
      )}
    </div>
  )
}

// 未配置 watchtower 时展示部署指引，替代更新按钮
function NotConfiguredNotice({ status }: { status: UpdateStatus | null }) {
  return (
    <div className="rounded-lg border border-amber-500/40 bg-amber-500/10 p-4 space-y-2">
      <div className="flex items-center gap-2 text-sm font-medium">
        <AlertTriangle className="h-4 w-4 text-amber-500" />
        一键更新未启用
      </div>
      <p className="text-sm text-muted-foreground">
        需要在服务器上启用 watchtower sidecar 并配置共享 token：
      </p>
      <ol className="text-sm text-muted-foreground list-decimal list-inside space-y-1">
        <li>在 <code className="font-mono">.env</code> 设置 <code className="font-mono">WATCHTOWER_HTTP_API_TOKEN</code>（可用 <code className="font-mono">openssl rand -hex 32</code> 生成）</li>
        <li>运行 <code className="font-mono">docker compose --profile updater up -d</code> 启动 watchtower sidecar</li>
        <li>确认 app 容器与 watchtower 在同一 Docker 网络（默认 compose 已保证）</li>
      </ol>
      {status && (
        <p className="text-xs text-muted-foreground">
          固定目标容器：<code className="font-mono">{status.container_name}</code>
        </p>
      )}
    </div>
  )
}

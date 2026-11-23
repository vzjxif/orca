import { useCallback, useEffect, useMemo, useState } from 'react'
import { Button, Card, CardBody, Input } from '@heroui/react'
import { Activity, Archive, ArrowDownToLine, ArrowLeft, ArrowUpRight, Bell, Box, Check, ChevronDown, CircleHelp, Clock3, Command, Container, FolderOpen, Gauge, HardDrive, Layers3, LoaderCircle, LogOut, Menu, MoreHorizontal, Pause, Play, RefreshCw, Search, Server, Settings, ShieldCheck, Square, Terminal, Trash2, TriangleAlert, X } from 'lucide-react'

type Project = { id: string; name: string; path: string; configDir: string; dataDir: string; composeFile: string; status: string; services: string[]; error?: string }
type ContainerInfo = { id: string; name: string; state: string; image: string }
type Task = { id: string; projectId?: string; projectName?: string; action?: string; includeData?: boolean; status?: string; createdAt?: string; error?: string }
type Backup = { id: string; name?: string; projectId?: string; projectName?: string; createdAt?: string; size?: number; path?: string; mode?: 'config' | 'config+data' | 'data-only' }
type LogLine = { line: string; stream?: string; time?: string }
type Page = 'overview' | 'tasks' | 'backups' | 'settings' | 'account'
type SavedNavigation = { page: Page; projectId?: string }
const navigationStorageKey = 'orca.navigation.v1'
const readSavedNavigation = (): SavedNavigation => {
  try {
    const value = JSON.parse(sessionStorage.getItem(navigationStorageKey) || 'null') as Partial<SavedNavigation> | null
    const pages: Page[] = ['overview', 'tasks', 'backups', 'settings', 'account']
    if (value && value.page && pages.includes(value.page)) return { page: value.page, projectId: typeof value.projectId === 'string' ? value.projectId : undefined }
  } catch { /* storage can be disabled; use the default page */ }
  return { page: 'overview' }
}
const clearSavedNavigation = () => { try { sessionStorage.removeItem(navigationStorageKey) } catch { /* storage can be disabled */ } }
type ActionName = 'start' | 'stop' | 'restart' | 'backup' | 'backup-data' | 'update'
type Notice = { kind: 'success' | 'error'; text: string }

const api = async <T,>(url: string, init: RequestInit = {}): Promise<T> => {
  const headers = new Headers(init.headers)
  if (init.body && !headers.has('Content-Type')) headers.set('Content-Type', 'application/json')
  const response = await fetch(url, { ...init, headers, credentials: 'include' })
  if (!response.ok) {
    let message = `${response.status} ${response.statusText}`
    try { const body = await response.json(); message = body.error || body.message || message } catch { /* response may not be JSON */ }
    throw new Error(message)
  }
  if (response.status === 204) return undefined as T
  return response.json() as Promise<T>
}
const actionLabel: Record<ActionName, string> = { start: '启动', stop: '停止', restart: '重启', backup: '配置备份', 'backup-data': '仅备份数据', update: '更新项目' }
const backupModeLabel = (mode?: Backup['mode']) => mode === 'config+data' ? '配置+数据备份' : mode === 'data-only' ? '仅数据备份' : '配置备份'
const taskLabel = (task: Task) => task.action === 'backup' ? (task.includeData ? '配置+数据备份' : '配置备份') : task.action === 'backup-data' ? '仅数据备份' : task.action === 'update' && task.includeData ? '更新项目（更新前备份数据）' : actionLabel[task.action as ActionName] || task.action || '—'
const statusLabel = (status?: string) => ({ running: '运行中', healthy: '运行正常', stopped: '已停止', exited: '已停止', restarting: '重启中', pending: '等待执行', queued: '排队中', running_task: '执行中', succeeded: '已完成', success: '已完成', completed: '已完成', failed: '失败', interrupted: '已中断', error: '异常' }[status || ''] || status || '未知')
const isRunning = (status?: string) => ['running', 'healthy'].includes((status || '').toLowerCase())
const prettyTime = (value?: string) => value ? new Date(value).toLocaleString('zh-CN', { hour12: false }) : '—'
const validateConfigDir = (value: string) => {
  const path = value.trim()
  if (!path) return '请输入项目内的相对目录，例如 config 或 settings/。'
  if (path.startsWith('/') || path.startsWith('\\') || /^[a-zA-Z]:/.test(path)) return '目录必须是项目内的相对路径，不能填写绝对路径。'
  if (path.includes('\\')) return '请使用正斜杠 / 作为目录分隔符。'
  if (path.split('/').some(part => part === '..')) return '目录不能使用 .. 跳出项目目录。'
  if (path.split('/').some(part => part.toLowerCase() === 'data')) return '为避免备份运行数据，目录路径不能包含 data 段。'
  if (path.includes(String.fromCharCode(0))) return '目录不能包含空字符。'
  return ''
}

const validateDataDir = (value: string) => {
  const path = value.trim()
  if (!path) return '请输入项目内的相对目录，例如 data 或 runtime/data。'
  if (path.startsWith('/') || path.startsWith(String.fromCharCode(92)) || /^[a-zA-Z]:/.test(path)) return '目录必须是项目内的相对路径，不能填写绝对路径。'
  if (path.includes(String.fromCharCode(92))) return '请使用正斜杠 / 作为目录分隔符。'
  if (path.split('/').some(part => part === '..')) return '目录不能使用 .. 跳出项目目录。'
  if (path.includes(String.fromCharCode(0))) return '目录不能包含空字符。'
  return ''
}

function App() {
  const [user, setUser] = useState<string | null>(null)
  const [authChecked, setAuthChecked] = useState(false)
  const [loginError, setLoginError] = useState('')
  const [loginSuccess, setLoginSuccess] = useState('')
  const [busy, setBusy] = useState(false)
  const [page, setPage] = useState<Page>('overview')
  const [navigationRestored, setNavigationRestored] = useState(false)
  const [projects, setProjects] = useState<Project[]>([])
  const [tasks, setTasks] = useState<Task[]>([])
  const [backups, setBackups] = useState<Backup[]>([])
  const [settings, setSettings] = useState<string[]>([])
  const [settingsDraft, setSettingsDraft] = useState('')
  const [settingsManagedByEnv, setSettingsManagedByEnv] = useState(false)
  const [selected, setSelected] = useState<Project | null>(null)
  const [containers, setContainers] = useState<ContainerInfo[]>([])
  const [logs, setLogs] = useState<LogLine[]>([])
  const [containerId, setContainerId] = useState('')
  const [logFilter, setLogFilter] = useState('')
  const [logTail, setLogTail] = useState('200')
  const [logsPaused, setLogsPaused] = useState(false)
  const [sseState, setSseState] = useState<'connecting' | 'connected' | 'disconnected' | 'paused'>('connecting')
  const [confirmAction, setConfirmAction] = useState<{ project: Project; action: ActionName } | null>(null)
  const [confirmDelete, setConfirmDelete] = useState<Backup | null>(null)
  const [notice, setNotice] = useState<Notice | null>(null)
  const [mobileNav, setMobileNav] = useState(false)
  const [search, setSearch] = useState('')
  const [scanBusy, setScanBusy] = useState(false)

  const notify = useCallback((kind: Notice['kind'], text: string) => { setNotice({ kind, text }); window.setTimeout(() => setNotice(null), 4200) }, [])
  const refreshProjects = useCallback(async () => { const result = await api<Project[]>('/api/projects'); const list = Array.isArray(result) ? result : []; setProjects(list); return list }, [])
  const refreshTasks = useCallback(async () => { const result = await api<Task[]>('/api/tasks'); setTasks(Array.isArray(result) ? result : []) }, [])
  const refreshBackups = useCallback(async () => { const result = await api<Backup[]>('/api/backups'); setBackups(Array.isArray(result) ? result : []) }, [])
  const refreshSettings = useCallback(async () => { const result = await api<{ roots: string[]; managedByEnv?: boolean }>('/api/settings'); setSettings(result.roots || []); setSettingsDraft((result.roots || []).join('\n')); setSettingsManagedByEnv(!!result.managedByEnv) }, [])

  useEffect(() => { api<{ username?: string; user?: string }>('/api/auth/me').then(me => setUser(me.username || me.user || '管理员')).catch(() => setUser(null)).finally(() => setAuthChecked(true)) }, [])
  useEffect(() => {
    if (!user) { setNavigationRestored(false); return }
    let live = true
    const saved = readSavedNavigation()
    void refreshProjects().then(items => {
      if (!live) return
      const savedProject = saved.projectId ? items.find(project => project.id === saved.projectId) : undefined
      if (saved.projectId && !savedProject) {
        setSelected(null)
        setPage('overview')
      } else {
        setSelected(savedProject || null)
        setPage(saved.page)
      }
      setNavigationRestored(true)
    }).catch(() => {
      if (!live) return
      setSelected(null)
      setPage(saved.projectId ? 'overview' : saved.page)
      setNavigationRestored(true)
    })
    void Promise.allSettled([refreshTasks(), refreshBackups(), refreshSettings()])
    return () => { live = false }
  }, [user, refreshProjects, refreshTasks, refreshBackups, refreshSettings])
  useEffect(() => {
    if (!user || !navigationRestored) return
    try { sessionStorage.setItem(navigationStorageKey, JSON.stringify({ page, projectId: selected?.id })) } catch { /* storage can be disabled */ }
  }, [user, navigationRestored, page, selected])
  useEffect(() => { if (!user) return; const timer = window.setInterval(() => { void refreshProjects().catch(() => undefined); void refreshTasks().catch(() => undefined) }, 5000); return () => window.clearInterval(timer) }, [user, refreshProjects, refreshTasks])
  useEffect(() => { const goSettings = () => { setSelected(null); setPage('settings') }; window.addEventListener('orca-nav-settings', goSettings); return () => window.removeEventListener('orca-nav-settings', goSettings) }, [])
  useEffect(() => { if (!selected) { setContainers([]); return }; let live = true; api<ContainerInfo[]>(`/api/projects/${encodeURIComponent(selected.id)}/containers`).then(items => { if (live) { setContainers(Array.isArray(items) ? items : []); if (!containerId && items[0]) setContainerId(items[0].id) } }).catch(error => { if (live) notify('error', `容器列表加载失败：${error.message}`) }); return () => { live = false } }, [selected, notify])
  useEffect(() => {
    if (!selected || !containerId || logsPaused) { setSseState(logsPaused ? 'paused' : 'disconnected'); return }
    setSseState('connecting')
    const params = new URLSearchParams({ container: containerId, tail: logTail })
    const source = new EventSource(`/api/projects/${encodeURIComponent(selected.id)}/logs?${params.toString()}`, { withCredentials: true })
    source.onopen = () => setSseState('connected')
    source.onmessage = event => { try { const item = JSON.parse(event.data) as LogLine; setLogs(current => [...current.slice(-499), item]) } catch { setLogs(current => [...current.slice(-499), { line: event.data }]) } }
    source.onerror = () => setSseState('disconnected')
    return () => source.close()
  }, [selected, containerId, logTail, logsPaused])

  const doLogin = async (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault(); setBusy(true); setLoginError('')
    const form = new FormData(event.currentTarget)
    try { const result = await api<{ username?: string; user?: string }>('/api/auth/login', { method: 'POST', body: JSON.stringify({ username: form.get('username'), password: form.get('password') }) }); setLoginSuccess(''); setUser(result.username || result.user || String(form.get('username') || '管理员')) }
    catch (error) { setLoginError(error instanceof Error ? error.message : '登录失败，请检查账号和密码') }
    finally { setBusy(false) }
  }
  const logout = async () => { try { await api('/api/auth/logout', { method: 'POST' }) } catch { /* clear local session even if the server already expired it */ }; clearSavedNavigation(); setNavigationRestored(false); setUser(null); setSelected(null); setPage('overview') }
  const changePassword = async (currentPassword: string, newPassword: string) => {
    await api('/api/auth/password', { method: 'POST', body: JSON.stringify({ currentPassword, newPassword }) })
    setSelected(null)
    clearSavedNavigation()
    setNavigationRestored(false)
    setLoginError('')
    setLoginSuccess('密码已修改。服务器已注销所有会话（包括当前会话），请重新登录。')
    setPage('overview')
    setUser(null)
  }
  const runAction = async (project: Project, action: ActionName, includeData = false) => {
    if (project.error) { notify('error', `项目存在错误，已禁用操作：${project.error}`); return }
    setBusy(true); setConfirmAction(null)
    const body = action === 'backup-data' ? { action } : action === 'backup' || action === 'update' ? { action, includeData } : { action }
    try {
      await api(`/api/projects/${encodeURIComponent(project.id)}/actions`, { method: 'POST', body: JSON.stringify(body) })
      notify('success', action === 'update' ? `“${project.name}”的更新任务已提交` : `“${project.name}”的${actionLabel[action]}任务已提交`)
      await Promise.allSettled([refreshTasks(), refreshProjects(), refreshBackups()])
    } catch (error) { notify('error', error instanceof Error ? error.message : '操作失败') } finally { setBusy(false) }
  }
  const scan = async () => { setScanBusy(true); try { await api('/api/scan', { method: 'POST' }); await refreshProjects(); notify('success', '扫描已完成，项目列表已更新') } catch (error) { notify('error', error instanceof Error ? error.message : '扫描失败') } finally { setScanBusy(false) } }
  const saveSettings = async () => { if (settingsManagedByEnv) { notify('error', '扫描目录由 Orca 服务环境变量 ORCA_PROJECTS_PATH 管理。'); return }; const roots = settingsDraft.split('\n').map(line => line.trim()).filter(Boolean); try { await api('/api/settings', { method: 'PUT', body: JSON.stringify({ roots }) }); setSettings(roots); notify('success', '扫描根目录已保存') } catch (error) { notify('error', error instanceof Error ? error.message : '保存失败') } }
  const saveProjectConfigDir = async (project: Project, configDir: string) => {
    const saved = await api<Project>(`/api/projects/${encodeURIComponent(project.id)}/config-dir`, { method: 'PUT', body: JSON.stringify({ configDir }) })
    const updated = { ...project, ...saved }
    setProjects(current => current.map(item => item.id === updated.id ? { ...item, ...updated } : item))
    setSelected(current => current?.id === updated.id ? { ...current, ...updated } : current)
    notify('success', `“${project.name}”的配置备份目录已保存为 ${saved.configDir || configDir}`)
  }
  const saveProjectDataDir = async (project: Project, dataDir: string) => {
    const saved = await api<Project>(`/api/projects/${encodeURIComponent(project.id)}/data-dir`, { method: 'PUT', body: JSON.stringify({ dataDir }) })
    const updated = { ...project, ...saved }
    setProjects(current => current.map(item => item.id === updated.id ? { ...item, ...updated } : item))
    setSelected(current => current?.id === updated.id ? { ...current, ...updated } : current)
    notify('success', `“${project.name}”的数据目录已保存为 ${saved.dataDir || dataDir}`)
  }
  const downloadBackup = async (backup: Backup) => { try { const response = await fetch(`/api/backups/${encodeURIComponent(backup.id)}/download`, { credentials: 'include' }); if (!response.ok) throw new Error(`${response.status} ${response.statusText}`); const blob = await response.blob(); const url = URL.createObjectURL(blob); const anchor = document.createElement('a'); anchor.href = url; anchor.download = backup.name || `orca-backup-${backup.id}.tar.gz`; anchor.click(); URL.revokeObjectURL(url) } catch (error) { notify('error', error instanceof Error ? error.message : '下载失败') } }
  const deleteBackup = async () => { if (!confirmDelete) return; try { await api(`/api/backups/${encodeURIComponent(confirmDelete.id)}`, { method: 'DELETE' }); setConfirmDelete(null); await refreshBackups(); notify('success', '备份已删除') } catch (error) { notify('error', error instanceof Error ? error.message : '删除失败') } }
  const filteredProjects = useMemo(() => projects.filter(p => `${p.name} ${p.path}`.toLowerCase().includes(search.toLowerCase())), [projects, search])
  const visibleLogs = useMemo(() => logs.filter(item => `${item.line} ${item.stream || ''}`.toLowerCase().includes(logFilter.toLowerCase())), [logs, logFilter])
  const runningCount = projects.filter(p => isRunning(p.status)).length
  const taskCount = tasks.filter(t => ['pending', 'queued', 'running', 'running_task'].includes((t.status || '').toLowerCase())).length

  if (!authChecked || (user && !navigationRestored)) return <div className="boot-screen"><LoaderCircle className="spin" size={24} />正在恢复 Orca 页面</div>
  if (!user) return <Login error={loginError} success={loginSuccess} busy={busy} onSubmit={doLogin} />

  const nav = (next: Page) => { setSelected(null); setPage(next); setMobileNav(false) }
  const openProject = (project: Project) => { setSelected(project); setContainerId(''); setLogs([]); setLogFilter(''); setLogsPaused(false) }
  return <div className="app-shell">
    <aside className={`sidebar ${mobileNav ? 'mobile-open' : ''}`}>
      <div className="brand"><div className="brand-icon" aria-hidden="true"><span className="dot-mark" /></div><div><strong>ORCA</strong><span>CONTAINER CONTROL</span></div></div>
      <div className="workspace"><div className="workspace-avatar"><span className="dot-mark" /></div><div><b>ORCA</b><span>LOCAL WORKSPACE</span></div><ChevronDown size={15} /></div>
      <div className="nav-caption">工作区</div>
      <nav className="nav-list">
        <NavItem icon={<Gauge size={17} />} label="项目概览" active={page === 'overview' && !selected} onClick={() => nav('overview')} />
        <NavItem icon={<Activity size={17} />} label="操作任务" badge={taskCount || undefined} active={page === 'tasks'} onClick={() => nav('tasks')} />
        <NavItem icon={<Archive size={17} />} label="配置备份" active={page === 'backups'} onClick={() => nav('backups')} />
      </nav>
      <div className="nav-caption nav-caption-spaced">管理</div>
      <nav className="nav-list"><NavItem icon={<Settings size={17} />} label="扫描设置" active={page === 'settings'} onClick={() => nav('settings')} /><NavItem icon={<ShieldCheck size={17} />} label="账户设置" active={page === 'account'} onClick={() => nav('account')} /></nav>
      <div className="sidebar-bottom"><div className="connection"><span className="pulse-dot"/><div><b>本地服务在线</b><span>API 连接正常</span></div><MoreHorizontal size={17}/></div><div className="user-row"><div className="user-avatar">{user.slice(0,1).toUpperCase()}</div><div className="user-meta"><b>{user}</b><span>管理员</span></div><button aria-label="退出登录" className="icon-button" onClick={() => void logout()}><LogOut size={17}/></button></div></div>
    </aside>
    {mobileNav && <button className="mobile-scrim" aria-label="关闭菜单" onClick={() => setMobileNav(false)} />}
    <main className="main-area">
      <header className="topbar"><div className="top-left"><button className="mobile-menu icon-button" onClick={() => setMobileNav(true)} aria-label="打开菜单"><Menu size={19}/></button><div className="breadcrumbs"><span>Orca</span><span className="crumb-slash">/</span>{selected ? <><button onClick={() => setSelected(null)}>项目</button><span className="crumb-slash">/</span><b>{selected.name}</b></> : <b>{page === 'overview' ? '项目概览' : page === 'tasks' ? '操作任务' : page === 'backups' ? '配置备份' : page === 'account' ? '账户设置' : '扫描设置'}</b>}</div></div><div className="top-actions"><span className="system-status"><span className="pulse-dot"/>系统正常</span><button className="icon-button notification" aria-label="通知"><Bell size={17}/><i/></button><div className="top-avatar">{user.slice(0,1).toUpperCase()}</div></div></header>
      <div className="content-scroll">
        {selected ? <ProjectDetail project={selected} containers={containers} containerId={containerId} setContainer={setContainerId} logs={visibleLogs} filter={logFilter} setFilter={setLogFilter} tail={logTail} setTail={value => { setLogTail(value); setLogs([]) }} paused={logsPaused} setPaused={setLogsPaused} streamState={sseState} onBack={() => setSelected(null)} onAction={action => setConfirmAction({ project: selected, action })} onSaveConfigDir={configDir => saveProjectConfigDir(selected, configDir)} onSaveDataDir={dataDir => saveProjectDataDir(selected, dataDir)} busy={busy} /> : page === 'overview' ? <Overview projects={filteredProjects} allCount={projects.length} runningCount={runningCount} taskCount={taskCount} backupCount={backups.length} search={search} setSearch={setSearch} onScan={() => void scan()} scanBusy={scanBusy} onSelect={openProject} onAction={(project, action) => setConfirmAction({ project, action })} busy={busy} /> : page === 'tasks' ? <Tasks tasks={tasks} projects={projects} onRefresh={() => void refreshTasks()} /> : page === 'backups' ? <Backups backups={backups} onDownload={downloadBackup} onDelete={setConfirmDelete} /> : page === 'settings' ? <SettingsPage draft={settingsDraft} setDraft={setSettingsDraft} roots={settings} managedByEnv={settingsManagedByEnv} onSave={() => void saveSettings()} /> : <AccountPage onChangePassword={changePassword} />}
      </div>
    </main>
    {notice && <div className={`toast ${notice.kind}`}><span>{notice.kind === 'success' ? <Check size={17}/> : <TriangleAlert size={17}/>}</span>{notice.text}<button onClick={() => setNotice(null)} aria-label="关闭"><X size={15}/></button></div>}
    {confirmAction && <ConfirmModal project={confirmAction.project} action={confirmAction.action} busy={busy} onCancel={() => setConfirmAction(null)} onConfirm={includeData => void runAction(confirmAction.project, confirmAction.action, includeData)} />}
    {confirmDelete && <div className="modal-backdrop" onMouseDown={event => { if (event.target === event.currentTarget) setConfirmDelete(null) }}><div className="confirm-modal"><div className="modal-icon danger"><Trash2 size={19}/></div><button className="modal-close icon-button" aria-label="关闭" onClick={() => setConfirmDelete(null)}><X size={18}/></button><h2>删除这份备份？</h2><p>删除后将无法恢复。备份文件 <strong>{confirmDelete.name || confirmDelete.id}</strong> 会被永久移除。</p><div className="modal-actions"><Button variant="flat" className="btn-secondary" onPress={() => setConfirmDelete(null)}>取消</Button><Button color="danger" className="btn-danger" onPress={() => void deleteBackup()}>确认删除</Button></div></div></div>}
  </div>
}

function Login({ error, success, busy, onSubmit }: { error: string; success: string; busy: boolean; onSubmit: (event: React.FormEvent<HTMLFormElement>) => void }) {
  return <div className="login-shell"><div className="login-visual"><div className="visual-glow"/><div className="visual-brand"><div className="brand-icon" aria-hidden="true"><span className="dot-mark" /></div><strong>ORCA</strong></div><div className="visual-copy"><span className="eyebrow"><span className="pulse-dot"/> YOUR CONTAINER COMMAND CENTER</span><h1>让每个容器，<br/>都在掌控之中。</h1><p>简洁、可靠的项目部署管理体验。<br/>从一处看见所有服务的运行状态。</p></div><div className="visual-footer"><span>© 2025 ORCA</span><span><ShieldCheck size={14}/> 安全的本地化管理</span></div><div className="orb orb-one"/><div className="orb orb-two"/></div><div className="login-panel"><form className="login-form" onSubmit={onSubmit}><div className="login-mark" aria-hidden="true"><span className="dot-mark" /></div><div className="login-heading"><h2>欢迎回来</h2><p>登录以继续管理你的容器项目</p></div>{success && <div className="password-success" role="status"><Check size={16}/>{success}</div>}<label className="field-label" htmlFor="username">用户名</label><Input id="username" name="username" type="text" autoComplete="username" placeholder="请输入用户名" isRequired variant="bordered" classNames={{ inputWrapper: 'login-input', input: 'login-input-inner' }} />
    <label className="field-label password-label" htmlFor="password">密码</label><Input id="password" name="password" type="password" autoComplete="current-password" placeholder="请输入密码" isRequired variant="bordered" classNames={{ inputWrapper: 'login-input', input: 'login-input-inner' }} />
    {error && <div className="login-error"><TriangleAlert size={15}/>{error}</div>}<Button type="submit" color="primary" isLoading={busy} className="login-submit">安全登录 <ArrowUpRight size={16}/></Button><div className="login-note"><ShieldCheck size={15}/> 你的登录信息通过安全会话保护</div></form><div className="login-copyright">Orca Container Console <span>·</span> 自托管控制台</div></div></div>
}
function NavItem({ icon, label, active, badge, onClick }: { icon: React.ReactNode; label: string; active: boolean; badge?: number; onClick: () => void }) { return <button className={`nav-item ${active ? 'active' : ''}`} onClick={onClick}>{icon}<span>{label}</span>{badge ? <em>{badge}</em> : null}</button> }
function PageTitle({ eyebrow, title, description, action }: { eyebrow?: string; title: string; description?: string; action?: React.ReactNode }) { return <div className="page-title-row"><div>{eyebrow && <div className="page-eyebrow">{eyebrow}</div>}<h1>{title}</h1>{description && <p>{description}</p>}</div>{action && <div className="page-title-action">{action}</div>}</div> }
function Overview({ projects, allCount, runningCount, taskCount, backupCount, search, setSearch, onScan, scanBusy, onSelect, onAction, busy }: { projects: Project[]; allCount: number; runningCount: number; taskCount: number; backupCount: number; search: string; setSearch: (value: string) => void; onScan: () => void; scanBusy: boolean; onSelect: (project: Project) => void; onAction: (project: Project, action: ActionName) => void; busy: boolean }) {
  return <div className="page-content"><PageTitle eyebrow="OVERVIEW / 项目总览" title="项目概览" description="一览所有 Compose 项目与服务运行状态。" action={<Button color="primary" className="btn-primary" onPress={onScan} isLoading={scanBusy}><RefreshCw size={15}/> 扫描项目</Button>}/>
    <div className="stats-grid"><StatCard label="全部项目" value={allCount} icon={<Layers3 size={18}/>} tone="teal" foot="已发现的 Compose 项目"/><StatCard label="运行中服务" value={runningCount} icon={<Activity size={18}/>} tone="blue" foot="当前处于活动状态"/><StatCard label="待处理任务" value={taskCount} icon={<Clock3 size={18}/>} tone="amber" foot="排队或正在执行"/><StatCard label="可用备份" value={backupCount} icon={<Archive size={18}/>} tone="violet" foot="已保存的配置快照"/></div>
    <div className="section-header"><div><h2>你的项目 <span className="count-pill">{allCount}</span></h2><p>选择项目查看容器和实时日志</p></div><div className="section-tools"><div className="search-box"><Search size={16}/><input value={search} onChange={e => setSearch(e.target.value)} placeholder="搜索项目名称或路径…"/><kbd>⌘ K</kbd></div><button className="filter-button"><span>全部状态</span><ChevronDown size={14}/></button></div></div>
    {projects.length ? <div className="project-grid">{projects.map(project => <ProjectCard key={project.id} project={project} onClick={() => onSelect(project)} onAction={onAction} busy={busy}/>)}</div> : <EmptyState icon={<FolderOpen size={24}/>} title={allCount ? '没有匹配的项目' : '还没有发现项目'} description={allCount ? '试试其他关键词，或清除搜索条件。' : '扫描已配置的根目录，Orca 会自动发现 Compose 项目。'} action={!allCount ? <Button color="primary" className="btn-primary" onPress={onScan} isLoading={scanBusy}><RefreshCw size={15}/> 扫描项目</Button> : undefined}/>}<div className="footer-note"><span><ShieldCheck size={14}/> 配置备份默认包含 Compose 文件、.env 与各项目所选配置目录；确认时可选择同时备份 data/ 数据目录。</span><a href="#settings" onClick={e => { e.preventDefault(); window.dispatchEvent(new CustomEvent('orca-nav-settings')) }}>了解备份范围 <ArrowUpRight size={13}/></a></div>
  </div>
}
function StatCard({ label, value, icon, tone, foot }: { label: string; value: number; icon: React.ReactNode; tone: string; foot: string }) { return <Card className="stat-card"><CardBody className="stat-body"><div className={`stat-icon ${tone}`}>{icon}</div><div className="stat-main"><span>{label}</span><strong>{value.toString().padStart(2, '0')}</strong></div><div className="stat-foot"><span className="stat-foot-dot"/>{foot}</div></CardBody></Card> }
function ProjectCard({ project, onClick, onAction, busy }: { project: Project; onClick: () => void; onAction: (project: Project, action: ActionName) => void; busy: boolean }) {
  const running = isRunning(project.status); const disabled = !!project.error
  return <Card className={`project-card ${disabled ? 'project-error' : ''}`}><CardBody className="project-card-body"><div className="project-card-top"><div className="project-symbol"><Box size={19}/></div><button className="card-more icon-button" aria-label="查看项目" onClick={onClick}><MoreHorizontal size={19}/></button></div><button className="project-title-button" onClick={onClick}><h3>{project.name}</h3><ArrowUpRight size={15}/></button><div className="project-path"><FolderOpen size={13}/><span title={project.path}>{project.path}</span></div><div className="project-divider"/><div className="project-meta"><span className={`status-badge ${disabled ? 'status-error' : running ? 'status-running' : 'status-stopped'}`}><i/>{disabled ? '配置错误' : statusLabel(project.status)}</span><span className="services-count"><Container size={14}/>{project.services?.length || 0} 个服务</span></div>{project.error && <div className="project-error-text" title={project.error}><TriangleAlert size={13}/>{project.error}</div>}<div className="project-card-bottom"><span><Server size={13}/>{project.composeFile || 'compose.yaml'}</span><div className="quick-actions"><Button isIconOnly size="sm" variant="flat" isDisabled={disabled || busy} aria-label="启动" onPress={() => onAction(project, 'start')}><Play size={14}/></Button><Button isIconOnly size="sm" variant="flat" isDisabled={disabled || busy} aria-label="停止" onPress={() => onAction(project, 'stop')}><Square size={13}/></Button></div></div></CardBody></Card>
}
function ProjectDetail({ project, containers, containerId, setContainer, logs, filter, setFilter, tail, setTail, paused, setPaused, streamState, onBack, onAction, onSaveConfigDir, onSaveDataDir, busy }: { project: Project; containers: ContainerInfo[]; containerId: string; setContainer: (id: string) => void; logs: LogLine[]; filter: string; setFilter: (value: string) => void; tail: string; setTail: (value: string) => void; paused: boolean; setPaused: (value: boolean) => void; streamState: string; onBack: () => void; onAction: (action: ActionName) => void; onSaveConfigDir: (configDir: string) => Promise<void>; onSaveDataDir: (dataDir: string) => Promise<void>; busy: boolean }) {
  const running = containers.filter(c => isRunning(c.state)).length; const disabled = !!project.error
  const [configDir, setConfigDir] = useState(project.configDir || 'config')
  const [configDirError, setConfigDirError] = useState('')
  const [configDirBusy, setConfigDirBusy] = useState(false)
  const [dataDir, setDataDir] = useState(project.dataDir || 'data')
  const [dataDirError, setDataDirError] = useState('')
  const [dataDirBusy, setDataDirBusy] = useState(false)
  useEffect(() => { setConfigDir(project.configDir || 'config'); setConfigDirError(''); setDataDir(project.dataDir || 'data'); setDataDirError('') }, [project.id, project.configDir, project.dataDir])
  const saveConfigDir = async (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    const validationError = validateConfigDir(configDir)
    if (validationError) { setConfigDirError(validationError); return }
    setConfigDirError(''); setConfigDirBusy(true)
    try { await onSaveConfigDir(configDir.trim().replace(/\/+$/, '')) }
    catch (error) { setConfigDirError(error instanceof Error ? `保存失败：${error.message}` : '保存失败，请稍后重试。') }
    finally { setConfigDirBusy(false) }
  }
  const saveDataDir = async (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    const validationError = validateDataDir(dataDir)
    if (validationError) { setDataDirError(validationError); return }
    setDataDirError(''); setDataDirBusy(true)
    try { await onSaveDataDir(dataDir.trim().replace(/\/+$/, '')) }
    catch (error) { setDataDirError(error instanceof Error ? `保存失败：${error.message}` : '保存失败，请稍后重试。') }
    finally { setDataDirBusy(false) }
  }
  return <div className="page-content detail-page"><button className="back-link" onClick={onBack}><ArrowLeft size={15}/> 返回项目列表</button><PageTitle eyebrow="PROJECT / 项目详情" title={project.name} description={project.path} action={<div className="detail-actions"><Button variant="bordered" className="btn-outline" isDisabled={disabled || busy} onPress={() => onAction('backup')}><Archive size={15}/> 备份配置</Button><Button variant="bordered" className="btn-outline" isDisabled={disabled || busy} onPress={() => onAction('backup-data')}><HardDrive size={15}/> 仅备份数据</Button><Button variant="bordered" className="btn-outline" isDisabled={disabled || busy} onPress={() => onAction('update')}><ArrowDownToLine size={15}/> 更新服务</Button><Button variant="bordered" className="btn-outline" isDisabled={disabled || busy} onPress={() => onAction('restart')}><RefreshCw size={15}/> 重启</Button><Button color="primary" className="btn-primary" isDisabled={disabled || busy} onPress={() => onAction(running ? 'stop' : 'start')}>{running ? <Square size={14}/> : <Play size={14}/>} {running ? '停止项目' : '启动项目'}</Button></div>}/>
    {project.error && <div className="error-banner"><TriangleAlert size={17}/><div><b>项目配置存在错误</b><span>{project.error}</span></div></div>}
    <div className="detail-summary"><div><span className="summary-icon"><Layers3 size={17}/></span><span>服务容器</span><b>{containers.length}</b></div><div><span className="summary-icon green"><Activity size={17}/></span><span>运行中</span><b>{running}</b></div><div><span className="summary-icon purple"><Command size={17}/></span><span>Compose 文件</span><b className="summary-file">{project.composeFile || 'compose.yaml'}</b></div><div><span className="summary-icon amber"><HardDrive size={17}/></span><span>项目目录</span><b className="summary-file" title={project.path}>{project.path}</b></div></div>
    <form className="panel config-dir-panel" onSubmit={saveConfigDir} noValidate><div className="config-dir-copy"><div className="settings-section-icon"><FolderOpen size={18}/></div><div><h2>配置备份目录</h2><p>当前目录：<code>{project.configDir || 'config'}</code></p><span>仅影响此项目的配置备份和更新前备份，不会修改 Compose 文件或服务配置。</span></div></div><div className="config-dir-form"><label htmlFor="config-dir">项目内相对目录</label><div className="config-dir-control"><input id="config-dir" value={configDir} onChange={event => { setConfigDir(event.target.value); if (configDirError) setConfigDirError('') }} placeholder="config" aria-invalid={!!configDirError} aria-describedby={configDirError ? 'config-dir-error' : 'config-dir-help'}/><Button type="submit" color="primary" className="btn-primary" isLoading={configDirBusy} isDisabled={configDirBusy}>保存目录 <Check size={15}/></Button></div><small id="config-dir-help">例如 <code>config</code> 或 <code>settings/</code>。必须位于项目目录内。</small>{configDirError && <div id="config-dir-error" className="config-dir-error" role="alert"><TriangleAlert size={14}/>{configDirError}</div>}</div></form>
    <form className="panel config-dir-panel" onSubmit={saveDataDir} noValidate><div className="config-dir-copy"><div className="settings-section-icon"><HardDrive size={18}/></div><div><h2>数据备份目录</h2><p>当前目录：<code>{project.dataDir || 'data'}</code></p><span>用于“仅备份数据”以及选择同时备份数据时；仅保存项目内的数据目录位置，不会修改服务配置。</span></div></div><div className="config-dir-form"><label htmlFor="data-dir">项目内相对目录</label><div className="config-dir-control"><input id="data-dir" value={dataDir} onChange={event => { setDataDir(event.target.value); if (dataDirError) setDataDirError('') }} placeholder="data" aria-invalid={!!dataDirError} aria-describedby={dataDirError ? 'data-dir-error' : 'data-dir-help'}/><Button type="submit" color="primary" className="btn-primary" isLoading={dataDirBusy} isDisabled={dataDirBusy}>保存目录 <Check size={15}/></Button></div><small id="data-dir-help">例如 <code>data</code> 或 <code>runtime/data</code>。必须位于项目目录内。</small>{dataDirError && <div id="data-dir-error" className="config-dir-error" role="alert"><TriangleAlert size={14}/>{dataDirError}</div>}</div></form>
    <div className="detail-columns"><section className="panel containers-panel"><div className="panel-heading"><div><h2>容器服务</h2><p>项目中的服务运行情况</p></div><span className="panel-count">{containers.length} 个</span></div>{containers.length ? <div className="container-list">{containers.map(container => <div className="container-row" key={container.id}><div className="container-mark"><Container size={17}/></div><div className="container-info"><b>{container.name}</b><span>{container.image}</span></div><span className={`status-badge ${isRunning(container.state) ? 'status-running' : 'status-stopped'}`}><i/>{statusLabel(container.state)}</span></div>)}</div> : <div className="inline-empty"><Container size={20}/><span>暂无容器服务</span></div>}</section>
      <section className="panel logs-panel"><div className="panel-heading logs-heading"><div><h2><Terminal size={17}/> 实时日志</h2><p><span className={`stream-dot ${streamState === 'connected' ? 'online' : ''}`}/>{streamState === 'connected' ? '实时连接中' : streamState === 'connecting' ? '正在连接…' : streamState === 'paused' ? '已暂停' : '连接中断，自动重连'}</p></div><div className="logs-tools"><select value={containerId} onChange={e => setContainer(e.target.value)} className="container-select" aria-label="选择容器"><option value="">选择容器</option>{containers.map(c => <option value={c.id} key={c.id}>{c.name}</option>)}</select><select value={tail} onChange={e => setTail(e.target.value)} className="container-select tail-select" aria-label="日志历史行数"><option value="100">最近 100 行</option><option value="200">最近 200 行</option><option value="500">最近 500 行</option><option value="1000">最近 1000 行</option></select><button className={`log-control ${paused ? 'paused' : ''}`} onClick={() => setPaused(!paused)} aria-label={paused ? '恢复日志' : '暂停日志'}>{paused ? <Play size={14}/> : <Pause size={14}/>}</button></div></div><div className="log-search"><Search size={14}/><input value={filter} onChange={e => setFilter(e.target.value)} placeholder="筛选日志关键词…"/><span>{logs.length} 行</span></div><div className="log-terminal">{logs.length ? logs.map((item, index) => <div className="log-line" key={`${item.time || ''}-${index}`}><span className="log-time">{item.time ? new Date(item.time).toLocaleTimeString('zh-CN', { hour12: false }) : '--:--:--'}</span><span className={`log-stream ${item.stream === 'stderr' ? 'stderr' : ''}`}>{item.stream || 'stdout'}</span><code>{item.line}</code></div>) : <div className="log-empty"><Terminal size={21}/><span>{paused ? '日志已暂停' : '等待容器日志…'}</span></div>}</div><div className="log-footer"><span><span className={`stream-dot ${streamState === 'connected' ? 'online' : ''}`}/> {streamState === 'connected' ? 'SSE 实时流' : 'SSE 自动重连中'}</span><button onClick={() => { /* EventSource reconnects automatically */ }}><RefreshCw size={12}/> 自动重连已启用</button></div></section></div>
  </div>
}
function Tasks({ tasks, projects, onRefresh }: { tasks: Task[]; projects: Project[]; onRefresh: () => void }) { const projectNames = new Map(projects.map(project => [project.id, project.name])); return <div className="page-content"><PageTitle eyebrow="ACTIVITY / 操作记录" title="操作任务" description="查看项目启动、更新和备份任务的执行进度。" action={<Button variant="bordered" className="btn-outline" onPress={onRefresh}><RefreshCw size={15}/> 刷新任务</Button>}/>{tasks.length ? <div className="panel table-panel"><div className="table-header"><span>任务 / 项目</span><span>操作</span><span>状态</span><span>创建时间</span></div>{tasks.map(task => <div className="table-row" key={task.id}><div className="task-project"><span className="table-icon"><Activity size={16}/></span><div><b>{task.projectName || projectNames.get(task.projectId || '') || task.projectId || 'Orca 任务'}</b><small>#{task.id}</small></div></div><span className="task-action">{taskLabel(task)}</span><span><TaskStatus status={task.status}/></span><span className="task-time">{prettyTime(task.createdAt)}</span>{task.error && <div className="task-error">{task.error}</div>}</div>)}</div> : <EmptyState icon={<Activity size={24}/>} title="暂时没有操作任务" description="当你启动、停止、更新或备份项目时，任务会显示在这里。"/>}</div> }
function TaskStatus({ status }: { status?: string }) { const s = (status || '').toLowerCase(); const tone = ['completed', 'success'].includes(s) ? 'status-running' : ['failed', 'error'].includes(s) ? 'status-error' : ['running', 'running_task'].includes(s) ? 'status-active' : 'status-stopped'; return <span className={`status-badge ${tone}`}><i/>{statusLabel(status)}</span> }
function Backups({ backups, onDownload, onDelete }: { backups: Backup[]; onDownload: (backup: Backup) => void; onDelete: (backup: Backup) => void }) { return <div className="page-content"><PageTitle eyebrow="SAFETY / 配置归档" title="配置备份" description="查看与下载项目的配置快照，随时恢复部署配置。" action={<span className="backup-scope"><ShieldCheck size={15}/> 配置备份默认不含数据目录，备份标签显示各自内容范围</span>}/>{backups.length ? <div className="backup-grid">{backups.map(backup => <Card className="backup-card" key={backup.id}><CardBody className="backup-card-body"><div className="backup-top"><div className="backup-icon"><Archive size={19}/></div><span className="backup-tag">{backupModeLabel(backup.mode)}</span><button className="icon-button backup-delete" title="删除备份" onClick={() => onDelete(backup)}><Trash2 size={15}/></button></div><h3>{backup.name || backup.projectName || `备份 ${backup.id}`}</h3><p>{backup.projectName || backup.projectId || 'Orca 项目'}</p><div className="backup-meta"><span><Clock3 size={13}/>{prettyTime(backup.createdAt)}</span><span><HardDrive size={13}/>{typeof backup.size === 'number' ? formatBytes(backup.size) : '—'}</span></div><Button variant="bordered" className="btn-outline backup-download" onPress={() => onDownload(backup)}><ArrowDownToLine size={15}/> 下载备份</Button></CardBody></Card>)}</div> : <EmptyState icon={<Archive size={24}/>} title="还没有配置备份" description="在项目详情中创建备份，或执行更新时自动生成更新前备份。"/>}<div className="backup-info"><ShieldCheck size={17}/><div><b>备份内容范围</b><p>配置备份包含主 Compose 文件、.env 与各项目所选配置目录；“配置+数据备份”和“仅数据备份”会额外打包项目的数据目录。</p></div></div></div> }
function formatBytes(bytes: number) { if (bytes < 1024) return `${bytes} B`; const kb = bytes / 1024; return kb < 1024 ? `${kb.toFixed(1)} KB` : `${(kb / 1024).toFixed(1)} MB` }
function AccountPage({ onChangePassword }: { onChangePassword: (currentPassword: string, newPassword: string) => Promise<void> }) {
  const [currentPassword, setCurrentPassword] = useState('')
  const [newPassword, setNewPassword] = useState('')
  const [confirmPassword, setConfirmPassword] = useState('')
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  const submit = async (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    setError('')
    const passwordBytes = new TextEncoder().encode(newPassword).byteLength
    if (passwordBytes < 12 || passwordBytes > 72) { setError('新密码长度必须为 12–72 个 UTF-8 字节。'); return }
    if (newPassword !== confirmPassword) { setError('两次输入的新密码不一致。'); return }
    setLoading(true)
    try {
      await onChangePassword(currentPassword, newPassword)
      setCurrentPassword(''); setNewPassword(''); setConfirmPassword('')
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : '修改密码失败，请稍后重试。')
    } finally { setLoading(false) }
  }

  return <div className="page-content account-page"><PageTitle eyebrow="ACCOUNT / 安全" title="账户设置" description="更新控制台账户密码，保护 Orca 管理界面。"/><div className="settings-layout"><form className="panel settings-panel account-password-panel" onSubmit={submit} noValidate><div className="settings-section-heading"><div className="settings-section-icon"><ShieldCheck size={18}/></div><div><h2>修改密码</h2><p>请输入当前密码并设置新的登录密码。</p></div></div><label className="field-label settings-label" htmlFor="currentPassword">当前密码</label><input className="account-password-input" id="currentPassword" name="currentPassword" type="password" autoComplete="current-password" required value={currentPassword} onChange={event => { setCurrentPassword(event.target.value); if (error) setError('') }} disabled={loading}/><label className="field-label settings-label" htmlFor="newPassword">新密码</label><input className="account-password-input" id="newPassword" name="newPassword" type="password" autoComplete="new-password" required value={newPassword} onChange={event => { setNewPassword(event.target.value); if (error) setError('') }} disabled={loading} aria-describedby="password-length-help"/><div id="password-length-help" className="settings-hint"><CircleHelp size={14}/> 长度为 12–72 个 UTF-8 字节。</div><label className="field-label settings-label" htmlFor="confirmPassword">确认新密码</label><input className="account-password-input" id="confirmPassword" name="confirmPassword" type="password" autoComplete="new-password" required value={confirmPassword} onChange={event => { setConfirmPassword(event.target.value); if (error) setError('') }} disabled={loading}/>{error && <div className="account-password-error" role="alert"><TriangleAlert size={15}/>{error}</div>}<div className="settings-footer account-password-footer"><span>提交后需要重新登录</span><Button type="submit" color="primary" className="btn-primary" isLoading={loading} isDisabled={loading || !currentPassword || !newPassword || !confirmPassword}>更新密码 <Check size={15}/></Button></div></form><aside className="settings-aside"><div className="aside-illustration"><ShieldCheck size={24}/><span/><span/><span/></div><h3>密码初始化与安全</h3><p>部署环境中的 <code>.env</code> 密码只用于首次初始化账户。账户创建后，请从这里修改密码；后续登录使用你设置的新密码。修改密码会注销所有会话，包括其他设备上的会话。</p></aside></div></div>
}
function SettingsPage({ draft, setDraft, roots, managedByEnv, onSave }: { draft: string; setDraft: (value: string) => void; roots: string[]; managedByEnv: boolean; onSave: () => void }) {
  return <div className="page-content settings-page"><PageTitle eyebrow="PREFERENCES / 扫描范围" title="扫描设置" description={managedByEnv ? '扫描目录由 Orca 服务环境变量提供。' : '管理 Orca 自动发现 Compose 项目的根目录。'}/><div className="settings-layout"><section className="panel settings-panel"><div className="settings-section-heading"><div className="settings-section-icon"><FolderOpen size={18}/></div><div><h2>项目扫描根目录</h2><p>{managedByEnv ? '由服务配置中的 ORCA_PROJECTS_PATH 管理。' : 'Orca 会在这些路径下发现 Compose 项目。每行填写一个绝对路径。'}</p></div></div><label className="field-label settings-label" htmlFor="roots">扫描目录路径</label><textarea id="roots" value={draft} onChange={event => setDraft(event.target.value)} readOnly={managedByEnv} placeholder={'/home/user/projects\n/opt/services'} rows={6}/><div className="settings-hint"><CircleHelp size={14}/>{managedByEnv ? '修改宿主机 .env 中的 ORCA_PROJECTS_PATH，并确保服务以同一路径挂载后重启 Orca。' : '支持多个目录；保存后点击“扫描项目”立即查找新项目。'}</div>{managedByEnv ? <div className="settings-footer"><span>由服务环境变量托管</span><code>ORCA_PROJECTS_PATH</code></div> : <div className="settings-footer"><span>{roots.length} 个已保存目录</span><Button color="primary" className="btn-primary" onPress={onSave}>保存设置 <Check size={15}/></Button></div>}</section><aside className="settings-aside"><div className="aside-illustration"><FolderOpen size={24}/><span/><span/><span/></div><h3>{managedByEnv ? '服务路径统一配置' : '保持项目井然有序'}</h3><p>{managedByEnv ? '扫描路径来自服务环境变量，并与 Compose 中的宿主机目录挂载保持一致。部署时配置一次，修改后重启服务生效。' : '设置一个或多个常用项目目录，Orca 将自动识别其中的 Docker Compose 项目。'}</p><div className="saved-roots"><b>当前扫描目录</b>{roots.length ? roots.map(root => <div key={root}><FolderOpen size={13}/><span title={root}>{root}</span></div>) : <span className="no-roots">尚未配置扫描目录</span>}</div></aside></div></div>
}
function EmptyState({ icon, title, description, action }: { icon: React.ReactNode; title: string; description: string; action?: React.ReactNode }) { return <div className="empty-state"><div className="empty-icon">{icon}</div><h3>{title}</h3><p>{description}</p>{action}</div> }
function ConfirmModal({ project, action, busy, onCancel, onConfirm }: { project: Project; action: ActionName; busy: boolean; onCancel: () => void; onConfirm: (includeData: boolean) => void }) {
  const [includeData, setIncludeData] = useState(false)
  const isUpdate = action === 'update'
  const isBackup = action === 'backup'
  const isDataBackup = action === 'backup-data'
  return <div className="modal-backdrop" onMouseDown={event => { if (event.target === event.currentTarget) onCancel() }}><div className="confirm-modal"><div className={`modal-icon ${action === 'stop' ? 'amber' : ''}`}>{isBackup || isDataBackup ? <Archive size={19}/> : action === 'stop' ? <Square size={17}/> : isUpdate ? <RefreshCw size={19}/> : <Play size={18}/>}</div><button className="modal-close icon-button" aria-label="关闭" onClick={onCancel}><X size={18}/></button><h2>确认{actionLabel[action]}？</h2><p>即将在项目 <strong>{project.name}</strong> 上执行此操作。</p>{isUpdate && <div className="modal-note"><ShieldCheck size={16}/><span>更新任务会先完成校验与配置备份，再拉取镜像、更新服务并检查健康状态。配置目录为 {project.configDir || 'config'}。</span></div>}{isBackup && <div className="modal-note"><ShieldCheck size={16}/><span>配置备份包含 Compose 文件、.env 与所选配置目录（{project.configDir || 'config'}）。数据目录为 {project.dataDir || 'data'}。</span></div>}{isDataBackup && <div className="modal-note"><HardDrive size={16}/><span>仅备份项目数据目录：{project.dataDir || 'data'}。</span></div>}{(isBackup || isUpdate) && <label className="modal-checkbox"><input type="checkbox" checked={includeData} onChange={event => setIncludeData(event.target.checked)}/><span>{isBackup ? '同时备份数据目录' : '更新前同时备份数据'}</span></label>}<div className="modal-actions"><Button variant="flat" className="btn-secondary" onPress={onCancel}>取消</Button><Button color="primary" className="btn-primary" isLoading={busy} onPress={() => onConfirm(includeData)}>确认{actionLabel[action]}</Button></div></div></div>
}

// Keep keyboard navigation useful for the common project search shortcut.
if (typeof window !== 'undefined') window.addEventListener('keydown', event => { if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === 'k') { event.preventDefault(); document.querySelector<HTMLInputElement>('.search-box input')?.focus() } })

export default App

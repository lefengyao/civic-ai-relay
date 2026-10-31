(() => {
  const root = document.getElementById('plugin-root')
  const style = document.createElement('style')
  style.textContent = `
    :root{color-scheme:dark;font-family:"Microsoft YaHei UI",sans-serif;background:#101724;color:#e7edf5}
    *{box-sizing:border-box}body{margin:0}#plugin-root{max-width:960px;margin:auto;padding:24px}
    h1{margin:8px 0;font-size:24px}p{color:#aab7c9;font-size:13px;line-height:1.7}.eyebrow{color:#e0bb77;letter-spacing:.15em;font-size:11px}
    .toolbar{display:flex;flex-wrap:wrap;gap:10px;margin:24px 0 16px}button,select{border:1px solid #39465a;background:#192436;color:inherit;border-radius:8px;padding:9px 12px;font:inherit;font-size:13px}
    select{flex:1;min-width:160px;max-width:100%}button{cursor:pointer}button:hover{border-color:#e0bb77}button:disabled{opacity:.45;cursor:default}
    button:focus-visible,select:focus-visible{outline:2px solid #e0bb77;outline-offset:3px}
    pre{background:#0b121d;border:1px solid #29374b;border-radius:10px;padding:18px;min-height:140px;max-height:65vh;overflow:auto;white-space:pre-wrap;overflow-wrap:anywhere;font:12px/1.8 Consolas,monospace}
    [role=status]{min-height:22px;margin:0}small{color:#91a0b7;font-size:12px;line-height:1.6}
    @media(max-width:480px){#plugin-root{padding:16px}.toolbar{gap:8px}select{flex-basis:100%}}
  `
  document.head.append(style)
  const element = (tag, text, parent = root) => {
    const node = document.createElement(tag)
    if (text) node.textContent = text
    parent.append(node)
    return node
  }
  element('div', 'CIVIC RELAY · DEPLOY').className = 'eyebrow'
  element('h1', 'Civic Relay')
  element('p', '查看中转服务容器的状态与日志。部署和更新请使用 Deploy 的 Civic Relay 项目操作。')
  const toolbar = element('div'); toolbar.className = 'toolbar'
  const select = element('select', '', toolbar); select.setAttribute('aria-label', 'Civic Relay 服务器')
  const reload = element('button', '刷新服务器', toolbar); reload.dataset.action = 'list'
  const status = element('button', '检查状态', toolbar); status.dataset.action = 'status'
  const logs = element('button', '查看日志', toolbar); logs.dataset.action = 'logs'
  const message = element('p'); message.setAttribute('role', 'status'); message.setAttribute('aria-live', 'polite')
  const output = element('pre', '选择服务器后检查状态，或查看最近 200 行日志。'); output.setAttribute('aria-label', '服务器结果')
  element('small', '管理控制台路径：/admin/。配置与数据库分别保存在 /app/config、/app/data 的命名卷中；更新时复用现有卷。健康检查由镜像内 civic-relay -healthcheck 执行。')
  let busy = false
  const updateControls = () => {
    select.disabled = busy || select.options.length === 0
    reload.disabled = busy
    status.disabled = logs.disabled = busy || select.options.length === 0
  }
  const run = async (task) => {
    if (busy) return
    busy = true; updateControls(); message.textContent = '读取中…'
    try { await task(); message.textContent = '已更新' }
    catch (error) { message.textContent = `读取失败：${error instanceof Error ? error.message : String(error)}` }
    finally { busy = false; updateControls() }
  }
  const loadServers = () => run(async () => {
    const previous = select.value
    const servers = await window.trailblazer.invoke('site.list')
    select.replaceChildren()
    for (const server of servers.filter((server) => server.siteProfileId === 'civic-relay')) {
      const option = element('option', server.label, select); option.value = server.id
    }
    if ([...select.options].some((option) => option.value === previous)) select.value = previous
    output.textContent = select.options.length ? '选择服务器后检查状态，或查看最近 200 行日志。' : '暂无 Civic Relay 服务器。请在 Deploy 新增服务器并选择 Civic Relay 站点。'
  })
  reload.addEventListener('click', loadServers)
  select.addEventListener('change', () => { output.textContent = '已切换服务器，请检查状态或读取日志。' })
  status.addEventListener('click', () => run(async () => {
    const state = await window.trailblazer.invoke('site.status', { serverId: select.value })
    const health = { none: '未配置', starting: '启动中', healthy: '正常', unhealthy: '异常' }
    output.textContent = `容器：${state.exists ? state.running ? '运行中' : '已停止' : '尚未部署'}\n健康检查：${health[state.health] ?? state.health}\n镜像：${state.image || '—'}`
  }))
  logs.addEventListener('click', () => run(async () => {
    output.textContent = await window.trailblazer.invoke('site.logs', { serverId: select.value, tail: 200 }) || '暂无日志。'
  }))
  updateControls()
  void loadServers()
})()


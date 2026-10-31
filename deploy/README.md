# Civic Relay 的 Deploy 插件

`civic-relay.json` 是本仓库拥有的站点部署契约；`civic-plugin/` 是专属面板源文件。独立 ZIP 插件包含站点配置和面板，手动添加一次即可接入 Civic。Deploy 不预装任何站点或补充功能插件，Civic 与 StoryFabric Web 均由用户自行安装。

## 导出与安装

```powershell
python scripts/export_deploy_plugin.py --sync-deploy
python scripts/package_site_zip.py -o dist/deploy/civic-relay-source.zip
```

第一条生成 `dist/deploy/civic-relay-plugin-1.0.0.zip`（插件）及 `civic-relay-plugin/`（可安装目录），并同步到相邻 Deploy 源码的可选示例 `examples/`，不放入内置站点目录或安装包。去掉 `--sync-deploy` 时只导出本仓库文件。这两条命令不会编译 Go 服务或生成 Deploy 安装包。

1. 解压 `civic-relay-plugin-1.0.0.zip`。确认解压后 `civic-relay-plugin/` 内只有 `manifest.json`、`panel.js`、`site.json` 三个文件；使用说明位于 ZIP 顶层。
2. Deploy 首页 →「管理功能插件」→「安装功能插件目录」，选择解压后的 `civic-relay-plugin/`。核对插件名称、版本、站点部署配置和权限后确认安装，站点与面板一并添加；只添加站点也可在「管理站点插件」导入 `site.json`。
3. 新增服务器并选择 Civic Relay，上传 `dist/deploy/civic-relay-source.zip`。部署器在服务器构建 Docker 镜像并检查镜像自身的健康状态。
4. 面板仅显示属于 `civic-relay` 的服务器；供应商、模型、Key 与配额仍在站点自身 `/admin/` 管理。

## 数据与构建契约

- Dockerfile：`Dockerfile.relay`；容器端口 8000；`/healthz` 存活探针。
- 使用镜像内 `/civic-relay -healthcheck`，适配 scratch/distroless 镜像，不依赖容器里的 shell/curl。
- `civic-relay_civic-relay-config` 挂载 `/app/config`；`civic-relay_civic-relay-data` 挂载 `/app/data`。更新复用这些卷。已有 compose 项目卷名不同时应先核对卷映射，不能直接换成空卷。
- 源码 ZIP 带 `.dockerignore`、Go 模块文件、cmd/internal/web；排除测试文件、relay.env、数据库、密钥和发布产物。
- 站点 JSON 和功能插件均不保存管理员密钥或供应商凭据。

服务器更新、compose 接管及反向代理说明见 [部署说明](../docs/deploy-with-trailblazer.md)。插件 ZIP 与 `civic-relay-source.zip` 用途不同：前者本机手动添加插件，后者是添加站点后上传服务器的部署源码。

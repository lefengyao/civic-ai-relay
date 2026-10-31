"""Export the site-owned Deploy contract and panel without building the application."""
import argparse
import json
from pathlib import Path
import shutil
import zipfile

ROOT = Path(__file__).resolve().parent.parent


def main():
    parser = argparse.ArgumentParser(description='导出 Civic Relay 的 Deploy 插件')
    parser.add_argument('--sync-deploy', action='store_true', help='同步到相邻的 trailblazer-deploy 源码')
    args = parser.parse_args()
    output = ROOT / 'dist/deploy'
    output.mkdir(parents=True, exist_ok=True)
    panel_output = output / 'civic-relay-plugin'
    panel_output.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(ROOT / 'deploy/civic-relay.json', output / 'civic-relay.json')
    for name in ('manifest.json', 'panel.js'):
        shutil.copyfile(ROOT / 'deploy/civic-plugin' / name, panel_output / name)
    shutil.copyfile(ROOT / 'deploy/civic-relay.json', panel_output / 'site.json')
    version = json.loads((panel_output / 'manifest.json').read_text(encoding='utf-8'))['version']
    if not isinstance(version, str) or not version or any(char not in '0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ.-' for char in version):
        raise SystemExit('插件版本号无效')
    archive = output / f'civic-relay-plugin-{version}.zip'
    with zipfile.ZipFile(archive, 'w', zipfile.ZIP_DEFLATED, compresslevel=9) as bundle:
        for name in ('manifest.json', 'panel.js', 'site.json'):
            bundle.write(panel_output / name, f'civic-relay-plugin/{name}')
        bundle.write(ROOT / 'deploy/README.md', 'README.zh-CN.md')
    with zipfile.ZipFile(archive) as bundle:
        if bundle.testzip() is not None:
            raise SystemExit('插件 ZIP 校验失败')
    if args.sync_deploy:
        deploy_root = ROOT.parent / 'trailblazer-deploy'
        if not (deploy_root / 'apps/desktop/package.json').is_file():
            raise SystemExit('未找到相邻的 Deploy 项目')
        examples = deploy_root / 'examples/site-plugins'
        examples.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(ROOT / 'deploy/civic-relay.json', examples / 'civic-relay.json')
        target = deploy_root / 'examples/feature-plugins/civic-relay'
        target.mkdir(parents=True, exist_ok=True)
        for name in ('manifest.json', 'panel.js', 'site.json'):
            shutil.copyfile(panel_output / name, target / name)
    print(f'站点 JSON：{output / "civic-relay.json"}')
    print(f'功能插件目录：{panel_output}')
    print(f'插件 ZIP：{archive}')


if __name__ == '__main__':
    main()

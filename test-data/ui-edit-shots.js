const path = require("path");
const fs = require("fs");
const { chromium } = require("playwright-core");

const OUT = "D:/生产项目/项目/AI工作流/civic-ai-relay/test-data";
const BASE = "http://127.0.0.1:8000";

(async () => {
  const exe = path.join(process.env.LOCALAPPDATA, "ms-playwright", "chromium-1208", "chrome-win64", "chrome.exe");
  const adminKey = fs.readFileSync("C:/ProgramData/CivicRelay/bootstrap-admin-key.txt", "utf8").trim();
  const browser = await chromium.launch({ executablePath: exe, headless: true });
  const page = await browser.newPage({ viewport: { width: 1440, height: 900 } });
  await page.goto(BASE + "/admin", { waitUntil: "networkidle" });
  await page.fill("#admin-key", adminKey);
  await page.click("#login-button");
  await page.waitForSelector("#shell.on", { timeout: 8000 });
  await page.waitForTimeout(800);

  // 供应商编辑弹窗
  await page.click('[data-view="providers"]');
  await page.waitForTimeout(600);
  await page.click("#provider-data .btn-ghost");
  await page.waitForTimeout(400);
  await page.screenshot({ path: path.join(OUT, "ui-6-edit-provider.png") });
  await page.click("#edit-cancel");

  // 模型组编辑弹窗（含成员预填）
  await page.click('[data-view="groups"]');
  await page.waitForTimeout(600);
  await page.click("#group-data .btn-ghost");
  await page.waitForTimeout(600);
  await page.screenshot({ path: path.join(OUT, "ui-7-edit-group.png") });
  await page.click("#edit-cancel");

  // Key 编辑弹窗
  await page.click('[data-view="keys"]');
  await page.waitForTimeout(600);
  await page.click("#key-data .btn-ghost");
  await page.waitForTimeout(600);
  await page.screenshot({ path: path.join(OUT, "ui-8-edit-key.png") });

  await browser.close();
  console.log("EDIT MODAL SHOTS DONE");
})().catch(e => { console.error("FAIL:", e.message); process.exit(1); });

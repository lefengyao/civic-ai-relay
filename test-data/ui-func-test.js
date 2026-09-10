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
  await page.waitForTimeout(600);
  // 打开模型组编辑，改名 + 勾选模型，保存
  await page.click('[data-view="groups"]');
  await page.waitForTimeout(600);
  await page.click("#group-data .btn-ghost");
  await page.waitForTimeout(600);
  await page.fill("#eg-name", "默认组-已改");
  await page.click("#edit-save");
  await page.waitForTimeout(800);
  // 校验：弹窗关闭且表格出现新名称
  const modalOpen = await page.evaluate(() => document.getElementById("edit-modal").classList.contains("on"));
  const tableText = await page.evaluate(() => document.getElementById("group-data").textContent);
  console.log("弹窗已关闭:", !modalOpen, "| 表格含新名称:", tableText.includes("默认组-已改"));
  await page.screenshot({ path: path.join(OUT, "ui-9-after-save.png") });
  // 改回原名
  await page.click("#group-data .btn-ghost");
  await page.waitForTimeout(600);
  await page.fill("#eg-name", "默认组");
  await page.click("#edit-save");
  await page.waitForTimeout(800);
  await browser.close();
  console.log("FUNC TEST DONE");
})().catch(e => { console.error("FAIL:", e.message); process.exit(1); });

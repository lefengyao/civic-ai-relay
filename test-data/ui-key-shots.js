const path = require("path");
const fs = require("fs");
const { chromium } = require("playwright-core");

const OUT = "D:/生产项目/项目/AI工作流/civic-ai-relay/test-data";
const BASE = "http://127.0.0.1:8000";

(async () => {
  const exe = path.join(process.env.LOCALAPPDATA, "ms-playwright", "chromium-1208", "chrome-win64", "chrome.exe");
  const adminKey = fs.readFileSync("C:/ProgramData/CivicRelay/bootstrap-admin-key.txt", "utf8").trim();
  const browser = await chromium.launch({ executablePath: exe, headless: true });
  const page = await browser.newPage({ viewport: { width: 1560, height: 980 } });
  await page.goto(BASE + "/admin", { waitUntil: "networkidle" });
  await page.fill("#admin-key", adminKey);
  await page.click("#login-button");
  await page.waitForSelector("#shell.on", { timeout: 8000 });

  // 1. 客户端 Key 页（密钥标识列 + 重置按钮）
  await page.click('[data-view="keys"]');
  await page.waitForTimeout(900);
  await page.screenshot({ path: path.join(OUT, "ui-15-keys-label.png") });
  const labels = await page.evaluate(() =>
    [...document.querySelectorAll("#key-data tbody tr")].map(r => r.children[2]?.textContent.trim()));
  console.log("密钥标识列:", labels);

  // 2. 点重置密钥（confirm 接受）
  page.on("dialog", d => d.accept());
  const firstRow = page.locator("#key-data tbody tr").first();
  await firstRow.locator("button", { hasText: "重置密钥" }).click();
  await page.waitForTimeout(1500);
  const modalOpen = await page.evaluate(() => document.getElementById("modal").classList.contains("on"));
  const token = await page.evaluate(() => document.getElementById("modal-token").textContent);
  const title = await page.evaluate(() => document.getElementById("modal-title").textContent);
  console.log("重置弹窗打开:", modalOpen, "| 标题:", title, "| 新密钥前缀:", token.slice(0, 12), "| 长度:", token.length);
  await page.screenshot({ path: path.join(OUT, "ui-16-rotate-token.png") });
  await page.click("#modal-close");
  await page.waitForTimeout(400);

  // 3. 回到 Key 页确认标识已刷新
  const labelsAfter = await page.evaluate(() =>
    [...document.querySelectorAll("#key-data tbody tr")].map(r => r.children[2]?.textContent.trim()));
  console.log("重置后标识列:", labelsAfter);
  console.log("列表未泄露明文:", !(await page.content()).includes(token));

  await browser.close();
  console.log("SHOTS DONE");
})().catch(e => { console.error("FAIL:", e.message); process.exit(1); });

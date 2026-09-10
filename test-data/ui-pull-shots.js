const path = require("path");
const fs = require("fs");
const { chromium } = require("playwright-core");

const OUT = "D:/生产项目/项目/AI工作流/civic-ai-relay/test-data";
const BASE = "http://127.0.0.1:8000";

(async () => {
  const exe = path.join(process.env.LOCALAPPDATA, "ms-playwright", "chromium-1208", "chrome-win64", "chrome.exe");
  const adminKey = fs.readFileSync("C:/ProgramData/CivicRelay/bootstrap-admin-key.txt", "utf8").trim();
  const browser = await chromium.launch({ executablePath: exe, headless: true });
  const page = await browser.newPage({ viewport: { width: 1440, height: 980 } });
  await page.goto(BASE + "/admin", { waitUntil: "networkidle" });
  await page.fill("#admin-key", adminKey);
  await page.click("#login-button");
  await page.waitForSelector("#shell.on", { timeout: 8000 });
  await page.waitForTimeout(600);

  // 1. 供应商页（含拉取模型按钮）
  await page.click('[data-view="providers"]');
  await page.waitForTimeout(900);
  await page.screenshot({ path: path.join(OUT, "ui-10-providers-pull.png") });

  // 2. 点本地假上游渠道的「拉取模型」
  const row = page.locator("#provider-data tr", { hasText: "本地测试渠道" });
  await row.locator("button", { hasText: "拉取模型" }).click();
  await page.waitForTimeout(1500);
  await page.screenshot({ path: path.join(OUT, "ui-11-pull-modal.png") });
  const listText = await page.evaluate(() => {
    const el = document.getElementById("pull-list");
    return el ? el.textContent.replace(/\s+/g, " ").slice(0, 200) : "(无列表)";
  });
  console.log("拉取弹窗列表:", listText);
  await page.click("#edit-cancel");
  await page.waitForTimeout(400);

  // 3. 模型组页（倍率列）
  await page.click('[data-view="groups"]');
  await page.waitForTimeout(900);
  await page.screenshot({ path: path.join(OUT, "ui-12-groups-rate.png") });

  // 4. 编辑分组弹窗（倍率输入）
  await page.click("#group-data .btn-ghost");
  await page.waitForTimeout(800);
  const rateValue = await page.inputValue("#eg-rate").catch(() => "(无倍率输入)");
  console.log("分组编辑倍率预填值:", rateValue);
  await page.screenshot({ path: path.join(OUT, "ui-13-group-rate-modal.png") });

  // 5. 端到端：改倍率为 2.0 并保存，校验表格刷新
  await page.fill("#eg-rate", "2.00");
  await page.click("#edit-save");
  await page.waitForTimeout(1200);
  const modalOpen = await page.evaluate(() => document.getElementById("edit-modal").classList.contains("on"));
  const tableText = await page.evaluate(() => document.getElementById("group-data").textContent);
  console.log("弹窗已关闭:", !modalOpen, "| 表格显示 ×2.00:", tableText.includes("2.00"));
  await page.screenshot({ path: path.join(OUT, "ui-14-rate-saved.png") });

  // 改回 1.50
  await page.click("#group-data .btn-ghost");
  await page.waitForTimeout(700);
  await page.fill("#eg-rate", "1.50");
  await page.click("#edit-save");
  await page.waitForTimeout(900);

  await browser.close();
  console.log("SHOTS DONE");
})().catch(e => { console.error("FAIL:", e.message); process.exit(1); });

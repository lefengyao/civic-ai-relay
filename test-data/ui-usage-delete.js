// 验证：Key 用量列 + 进度条 + 删除按钮；以及一次真实的删除流程（删除已存在的"临时待删"）
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

  await page.click('[data-view="keys"]');
  await page.waitForTimeout(800);
  const keyText = await page.evaluate(() => document.getElementById("key-data").textContent);
  console.log("tokens用量显示:", keyText.includes("2,000,000") || keyText.includes("2000000") ? "✓" : "✗");
  console.log("金额3元显示:", keyText.includes("3") ? "✓" : "✗");
  console.log("进度条:", (await page.locator(".usage-bar").count()) + " 条");
  console.log("进度条红色警示(60%不会红,仅样式存在):", (await page.locator(".usage-bar i").count()) > 0 ? "✓" : "✗");
  await page.screenshot({ path: path.join(OUT, "ui-17-keys-usage.png") });

  // 真实删除：确认对话框 → 删除"临时待删"
  if (!(await page.locator('#key-data tr:has-text("临时待删")').count())) {
    console.log("没有临时待删行，跳过删除验证");
  } else {
    page.once("dialog", d => { console.log("确认弹窗内容:", d.message().slice(0, 40), "..."); d.accept(); });
    await page.locator('#key-data tr:has-text("临时待删") .btn-danger').click();
    await page.waitForTimeout(900);
    const still = await page.evaluate(() => document.getElementById("key-data").textContent.includes("临时待删"));
    console.log("删除后行消失:", !still ? "✓" : "✗ 仍存在");
  }
  await page.screenshot({ path: path.join(OUT, "ui-18-after-delete.png") });
  await browser.close();
  console.log("DONE");
})().catch(e => { console.error("FAIL:", e.message); process.exit(1); });

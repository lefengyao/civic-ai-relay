const path = require("path");
const fs = require("fs");
const { chromium } = require("playwright-core");

const OUT = "D:/生产项目/项目/AI工作流/civic-ai-relay/test-data";
const BASE = "http://127.0.0.1:8000";

(async () => {
  const exeRoot = path.join(process.env.LOCALAPPDATA, "ms-playwright", "chromium-1208");
  const exe = path.join(exeRoot, "chrome-win64", "chrome.exe");
  if (!fs.existsSync(exe)) throw new Error("chromium not found: " + exe);

  const adminKey = fs.readFileSync("C:/ProgramData/CivicRelay/bootstrap-admin-key.txt", "utf8").trim();

  const browser = await chromium.launch({ executablePath: exe, headless: true });
  const page = await browser.newPage({ viewport: { width: 1440, height: 900 } });

  await page.goto(BASE + "/admin", { waitUntil: "networkidle" });
  await page.screenshot({ path: path.join(OUT, "ui-1-login.png") });
  console.log("login screenshot done");

  await page.fill("#admin-key", adminKey);
  await page.click("#login-button");
  await page.waitForSelector("#shell.on", { timeout: 8000 });
  await page.waitForTimeout(1200);
  await page.screenshot({ path: path.join(OUT, "ui-2-overview.png") });
  console.log("overview screenshot done");

  for (const [view, name] of [["providers", "ui-3-providers"], ["models", "ui-4-models"], ["keys", "ui-5-keys"]]) {
    await page.click(`[data-view="${view}"]`);
    await page.waitForTimeout(900);
    await page.screenshot({ path: path.join(OUT, name + ".png") });
    console.log(name, "screenshot done");
  }

  await browser.close();
  console.log("ALL DONE");
})().catch(e => { console.error("FAIL:", e.message); process.exit(1); });

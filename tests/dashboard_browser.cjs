const { chromium } = require("playwright");
(async () => {
  const browser = await chromium.launch({headless:true});
  const page = await browser.newPage({viewport:{width:1920,height:1080}});
  const errors=[];
  page.on("pageerror", error=>errors.push(error.message));
  try {
    await page.goto("http://grafana:3000/login");
    await page.getByPlaceholder("email or username").fill("admin");
    await page.getByPlaceholder("password", {exact:true}).fill("isolated-test-password");
    await page.getByRole("button",{name:"Log in",exact:true}).click();
    await page.waitForURL(url=>!url.pathname.endsWith("/login"));
    await page.goto("http://grafana:3000/d/ts-funnel-services?var-datasource=ts-funnel-test&var-job=ts-funnel&var-environment=All&var-host=All&var-service=grafana&var-instance=All&var-component=tailscale&from=now-24h&to=now");
    await page.getByText("immich",{exact:true}).first().waitFor({timeout:90000});
    for (const text of ["Unreachable","Unavailable","Health","Current","Previous","Last successful update","paperless-1","paperless-2"]) {
      await page.getByText(text,{exact:true}).first().waitFor({timeout:30000});
    }
    await page.screenshot({path:"/tmp/dashboard.png",fullPage:true});
    await page.getByText("paperless",{exact:true}).first().click();
    await page.waitForURL(url=>url.searchParams.get("var-service")==="paperless",{timeout:30000});
    const url=new URL(page.url());
    if (!url.pathname.startsWith("/d/ts-funnel-services")) throw Error("Detail link left the dashboard");
    if (!url.searchParams.get("var-instance")?.startsWith("paperless-")) throw Error("Detail link lost replica identity");
    if (url.searchParams.get("var-job")!=="ts-funnel" || url.searchParams.get("var-datasource")!=="ts-funnel-test" || !url.searchParams.get("from")) throw Error("Detail link lost filters or time range");
    await page.getByText("immich",{exact:true}).first().waitFor({timeout:30000});
    console.log(JSON.stringify({browser_table:true,detail_link:true,fleet_preserved:true,page_errors:errors}));
    if (errors.length) throw Error(errors.join("; "));
  } catch(error) {
    console.error(error.message);
    console.error((await page.locator("body").innerText()).slice(0,12000));
    throw error;
  } finally {await browser.close();}
})().catch(()=>process.exit(1));

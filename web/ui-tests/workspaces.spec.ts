import { test, expect, type Page } from '@playwright/test';
import { mkdirSync } from 'node:fs';
import { join } from 'node:path';
const output = join(process.cwd(), '../data/verification/ui-refactor');
const timestamp = '2026-10-07T00:00:00+08:00';
type Actor = 'platform' | 'owner' | 'member' | 'suspended' | 'personal' | 'anonymous';
async function mockSession(page: Page, actor: Actor) {
  const user = {id:'7',email:'demo@example.test',status:'active',platform_admin:actor==='platform',created_at:timestamp,updated_at:timestamp};
  const personal = {organization:{id:'1',type:'personal',name:'我的个人账户',status:'active',created_at:timestamp},role:'owner',membership_id:'1',status:'active',monthly_limit_micro:null};
  const enterprise = {organization:{id:'2',type:'enterprise',name:'青禾科技',status:'active',created_at:timestamp},role:actor==='owner'?'owner':'member',membership_id:'2',status:actor==='suspended'?'suspended':'active',monthly_limit_micro:'200000000'};
  const forbiddenRequests: string[] = [];
  await page.route('**/api/v1/**', async route => {
    const path = new URL(route.request().url()).pathname;
    let data: unknown; let status = 200;
    if (path === '/api/v1/me') {
      if (actor==='anonymous') { status=401; data={error:{code:'unauthorized',message:'login required',request_id:'ui-fixture'}}; }
      else data={user,organizations:actor==='personal'?[personal]:[personal,enterprise],csrf_token:'ui-fixture-only'};
    } else if(path==='/api/v1/admin/billing/summary') {
      if(actor!=='platform') forbiddenRequests.push(path);
      data={month:'2026-10',sales_micro:'2465500000',known_provider_cost_micro:'854200000',unknown_provider_cost_count:3,settled_count:492,pending_count:2,released_count:1,review_required_count:1,prompt_tokens:1352000,completion_tokens:482000,total_tokens:1834000,unknown_usage_count:0};
    } else if(path.endsWith('/billing/account')) {
      if(actor==='member'||actor==='suspended') forbiddenRequests.push(path);
      data={organization_id:'2',currency:'CNY',available_micro:'1735800000',posted_balance_micro:'1737050000',reserved_micro:'1250000',status:'active',version:'1',created_at:timestamp,updated_at:timestamp,available_note:''};
    } else if(path.endsWith('/quota')) data={organization_id:'2',user_id:'7',month:'2026-10',limit_micro:actor==='owner'?null:'200000000',used_micro:'64520000',reserved_micro:'1250000',remaining_micro:actor==='owner'?null:'134230000'};
    else if(path.endsWith('/usage')) data={items:[],month:'2026-10',scope:actor==='owner'?'organization':'self',next_cursor:null,totals:{sales_micro:actor==='owner'?'176240000':'64520000',settled_count:138,pending_count:2,review_required_count:0,prompt_tokens:27000,completion_tokens:16000,total_tokens:43000,observed_prompt_tokens:27000,observed_completion_tokens:16000,observed_total_tokens:43000,unknown_usage_count:0}};
    else if(path.endsWith('/models')) {
      if(actor==='suspended') forbiddenRequests.push(path);
      data={organization_id:'2',effective_models:['gpt-demo','claude-demo','deepseek-demo'].map(model=>({model,input_price_micro_per_million:'1000000',output_price_micro_per_million:'3000000'}))};
    } else if(path.endsWith('/keys')) data={items:[{key_id:'demo',name:'我的聊天软件',prefix:'demo-prefix…',organization_id:'2',owner_user_id:'7',created_by:'7',status:'active',expires_at:null,revoked_at:null,model_allowlist:null,policy_version:'1',created_at:timestamp,tenant_id:'2'}],next_cursor:null};
    else { forbiddenRequests.push(path); status=404; data={error:{code:'not_found',message:'fixture not defined',request_id:'fixture'}}; }
    await route.fulfill({status,contentType:'application/json',body:JSON.stringify(data)});
  });
  return forbiddenRequests;
}
async function capture(page:Page,name:string,project:string) {
  await expect(page.locator('.ant-spin-spinning')).toHaveCount(0);
  await page.evaluate(()=>document.fonts.ready);
  expect(await page.evaluate(()=>document.documentElement.scrollWidth)).toBeLessThanOrEqual(page.viewportSize()!.width+1);
  mkdirSync(output,{recursive:true});
  await page.screenshot({path:join(output,`${project}-${name}.png`),fullPage:true,animations:'disabled'});
}
test('platform starts in platform workspace and can explicitly enter personal usage',async({page},info)=>{
  const forbidden=await mockSession(page,'platform');await page.goto('/');
  await expect(page).toHaveURL(/platform\/overview$/);
  await expect(page.getByRole('heading',{name:'平台工作台'})).toBeVisible();
  await expect(page.getByLabel('切换账户')).toHaveCount(0);
  await capture(page,'platform',info.project.name);
  await page.getByRole('button',{name:'我的使用账户'}).click();
  await expect(page).toHaveURL(/orgs\/1\/overview$/);
  await expect(page.getByRole('heading',{name:'我的工作台'})).toBeVisible();
  await expect(page.getByRole('button',{name:'进入平台管理'})).toBeVisible();
  expect(forbidden).toEqual([]);
});
test('enterprise owner sees wallet and team workflow',async({page},info)=>{
  const forbidden=await mockSession(page,'owner');await page.goto('/orgs/2/overview');
  await expect(page.getByRole('heading',{name:'企业工作台'})).toBeVisible();
  await expect(page.getByTestId('overview-balance')).toBeVisible();
  await expect(page.getByRole('button',{name:'管理企业成员'})).toBeVisible();
  await capture(page,'owner',info.project.name);expect(forbidden).toEqual([]);
});
test('enterprise member sees own quota and no enterprise wallet or management',async({page},info)=>{
  const forbidden=await mockSession(page,'member');await page.goto('/orgs/2/overview');
  await expect(page.getByRole('heading',{name:'我的工作台'})).toBeVisible();
  await expect(page.getByTestId('overview-quota')).toContainText('本月还能使用');
  await expect(page.getByTestId('overview-balance')).toHaveCount(0);
  await capture(page,'member',info.project.name);
  if(info.project.name==='mobile')await page.getByRole('button',{name:'打开导航'}).click();
  await expect(page.getByRole('menuitem',{name:'成员与申请'})).toHaveCount(0);
  await expect(page.getByRole('menuitem',{name:'企业账单'})).toHaveCount(0);
  expect(forbidden).toEqual([]);
});
test('suspended member retains history without a misleading key CTA or models request',async({page})=>{
  const forbidden=await mockSession(page,'suspended');await page.goto('/orgs/2/overview');
  await expect(page.getByText('你在此企业中的成员资格已暂停')).toBeVisible();
  await expect(page.getByRole('button',{name:'获取 API Key'})).toHaveCount(0);
  await expect(page.getByRole('link',{name:/选择与试用模型/})).toHaveCount(0);
  await expect(page.getByTestId('overview-quota')).toBeVisible();
  expect(forbidden).toEqual([]);
});
test('keys explain the connection settings without exposing existing secrets',async({page},info)=>{
  const forbidden=await mockSession(page,'member');await page.goto('/orgs/2/keys');
  await expect(page.getByText('如何接入聊天软件或应用？')).toBeVisible();
  await expect(page.getByText('http://127.0.0.1:5210/v1',{exact:true})).toBeVisible();
  await expect(page.getByRole('button',{name:'新建 Key'})).toBeVisible();
  await capture(page,'keys',info.project.name);expect(forbidden).toEqual([]);
});
test('login introduces the product without overflowing on narrow screens',async({page},info)=>{
  await mockSession(page,'anonymous');await page.goto('/login');
  await expect(page.getByRole('heading',{name:'欢迎回来'})).toBeVisible();
  await expect(page.getByLabel('邮箱',{exact:true})).toBeEnabled();
  await capture(page,'login',info.project.name);
});

test('model trial fields are clear and an intercepted streaming reply is rendered', async ({page},info) => {
  const forbidden=await mockSession(page,'member');
  await page.route('**/v1/chat/completions',route=>route.fulfill({contentType:'text/event-stream',body:'data: {"choices":[{"delta":{"content":"这是模拟回答"}}]}\n\ndata: [DONE]\n\n'}));
  await page.goto('/orgs/2/models');
  await expect(page.getByTestId('account-models')).toBeVisible();
  await page.getByLabel('我的 API Key',{exact:true}).fill('ui-fixture-token');
  await page.getByRole('button',{name:'启用 Key',exact:true}).click();
  await page.getByLabel('选择模型',{exact:true}).click();
  await page.locator('.ant-select-item-option-content').filter({hasText:'gpt-demo'}).click();
  await page.getByLabel('试用内容').fill('你好');
  await page.getByRole('button',{name:'发送',exact:true}).click();
  await expect(page.getByText('回答完成',{exact:true})).toBeVisible();
  await expect(page.getByRole('button',{name:'发送',exact:true})).toBeEnabled();
  await page.keyboard.press('Escape');
  await expect(page.locator('.playground-output')).toContainText('这是模拟回答');
  await capture(page,'models',info.project.name);
  expect(forbidden).toEqual([]);
});
test('non-platform user cannot load the platform dashboard or request platform totals',async({page})=>{
  const forbidden=await mockSession(page,'member');await page.goto('/platform/overview');
  await expect(page.getByText('此页面仅供平台管理员使用',{exact:true})).toBeVisible();
  await expect(page.getByTestId('platform-overview-summary')).toHaveCount(0);
  expect(forbidden).toEqual([]);
});

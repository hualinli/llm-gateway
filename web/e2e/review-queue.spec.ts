// The real authenticated app renders a controlled queue fixture. Money movement
// and audit invariants are covered by the HTTP + PostgreSQL integration suite.
import {AUTH,expect,loadState,requireBackend,test} from './fixtures';
const state=loadState();
test.describe('manual review operator UI',()=>{
 test.use({storageState:AUTH.admin});test.beforeEach(()=>requireBackend(state));
 test('requires a reason and an explicit charge confirmation, then shows committed money',async({page})=>{
  let handled=false;let writes=0;
  await page.route('**/api/v1/admin/billing/review-required*',async route=>{
   await route.fulfill({json:{items:handled?[]:[{execution_id:'exec_ui_review',organization_id:state.orgA.id,model:'review',reserved_micro:'150000',updated_at:'2026-10-01T00:00:00Z',age_seconds:3600,sla_breached:false,reason:'missing usage'}],next_cursor:null,now:'2026-10-01T01:00:00Z',totals:{open:handled?'0':'1',sla_breaches:'0',oldest_age_seconds:3600},min_age_seconds:2400}});
  });
  await page.route('**/api/v1/admin/billing/review-required/exec_ui_review/settle',async route=>{
   expect(route.request().postDataJSON()).toEqual({reason:'provider verified'});expect(route.request().headers()['x-csrf-token']).toBeTruthy();writes++;handled=true;
   await route.fulfill({json:{execution_id:'exec_ui_review',organization_id:state.orgA.id,status:'settled',action:'settle',charge_micro:'150000',policy:'reviewer_charges_the_reservation_pinned_at_admission',account:{available_micro:'9850000',reserved_micro:'0'},debit_entry:{id:'81'},release_entry:{id:'82'},request_id:'review-ui-1'}});
  });
  await page.goto('/platform/billing');const card=page.getByTestId('review-queue-card');await card.getByRole('button',{name:'人工结算',exact:true}).click();
  const modal=page.getByRole('dialog');await modal.getByRole('button',{name:'确认扣款并结清'}).click();expect(writes).toBe(0);
  await modal.getByLabel('处理理由（记录到审计）').fill('provider verified');await modal.getByRole('button',{name:'确认扣款并结清'}).click();
  await expect(modal).not.toBeVisible();await expect(card.getByText('请求 exec_ui_review 已人工结算')).toBeVisible();await expect(card.getByText('review-ui-1')).toBeVisible();expect(writes).toBe(1);
 });
});

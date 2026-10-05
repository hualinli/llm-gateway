/**
 * Organization billing (`/orgs/:org/billing`) — the T08 "资金与结算" surface.
 *
 * Read scope is the server-side `view_org_usage` action: owner, admin and the
 * read-only `billing_viewer`. A plain `member` is `403 forbidden` **by the
 * permission matrix**, and this page proves it instead of hiding the tabs: the
 * requests are still issued and the real `403` is rendered.
 *
 * No number on this page is derived client-side. Totals come from the server's
 * `totals` block, the ledger is append-only, and the frozen wording fields are
 * shown verbatim.
 */

import type { ReactNode } from 'react';
import { Alert, Card, Space, Tag, Typography } from 'antd';

import { ContractGap, PageHeader } from '../components/PageHeader';
import {
  BillingAccountCard,
  BillingEntriesSection,
  BillingExceptionsSection,
  BillingStatementSection,
} from '../components/BillingSections';
import { useActiveOrg } from '../session/SessionProvider';
import { CAN, ROLE_LABELS } from '../utils/permissions';
import { Link } from 'react-router-dom';
import { OrganizationContext } from '../components/ProductGuide';

export function BillingPage(): ReactNode {
  const { orgId, role } = useActiveOrg();
  const canRead = CAN.viewOrgUsage(role);

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <PageHeader
        title="资金与结算"
        description={`查看模型 API 调用产生的费用、余额与月度账单 · ${role ? ROLE_LABELS[role] : '—'}`}
        extra={
          <Space size={4} wrap>
            <Tag color={canRead ? 'green' : 'red'}>
              view_org_usage {canRead ? '允许' : '不允许'}
            </Tag>
            {role === 'billing_viewer' ? <Tag color="purple">只读（billing_viewer）</Tag> : null}
          </Space>
        }
      />

      <Alert
        type="info"
        showIcon
        message="从模型调用，到用量和账单"
        description={
          <span>
            应用通过 API Key 调用模型；在途调用先预占额度，取得可计费的用量后再结算。
            这里核对的是当前组织的资金与消费记录。{' '}
            <Link to={`/orgs/${encodeURIComponent(orgId ?? '')}/overview`}>查看平台介绍与使用步骤 →</Link>
          </span>
        }
      />
      <OrganizationContext />

      {!canRead ? (
        <Alert
          type="warning"
          showIcon
          message="普通成员没有组织账单读取权限（服务端 403）"
          description="ADR-0003 §2.3 的矩阵把资金读取限定为 owner / admin / billing_viewer。下面每个卡片都会发出真实请求并显示服务端返回的 403 forbidden（含 code 与 request_id）——这正是 RBAC 的证明：隐藏卡片不是授权，服务端才是唯一执行点。成员自己的消费与请求明细在「用量」页（scope=self）。"
        />
      ) : null}

      <BillingAccountCard org={orgId} enabled={orgId !== null} />

      <BillingStatementSection org={orgId} />

      <BillingEntriesSection org={orgId} />

      <BillingExceptionsSection org={orgId} canResolve={false} />

      <Card size="small" title="本页标签与口径">
        <Space direction="vertical" size={4}>
          <Typography.Text>
            <strong>posted_balance_micro</strong>：所有已入账分录之和，可为负（显式债务，admission 拒绝从其消费）。
          </Typography.Text>
          <Typography.Text>
            <strong>reserved_micro</strong>：在途执行按 T06 预占的资金，永不为负。
          </Typography.Text>
          <Typography.Text>
            <strong>available_micro</strong> = posted − reserved，服务端回显，客户端不重算。
          </Typography.Text>
          <Typography.Text>
            <strong>occurred_at vs posted_at</strong>：前者是用量发生的时刻（用量分析），后者是钱移动的时刻（本页唯一的合计轴）；迟到结算记入实际 posted 月，并单独标出 late_entries/late_days。
          </Typography.Text>
        </Space>
      </Card>

      <ContractGap title="没有「单次请求结算状态」与「组织可见价目」端点">
        {'1) 契约没有 `GET /executions/{id}` 之类的执行详情端点：请求详情无法查询一次执行最终被记了多少钱、receipt 处于哪个状态；账本里的 settlement 分录 `business_id` 形如 '}
        <code>billing_receipt:debit:&#123;execution&#125;</code>
        {'，但 entries 端点只有 kind 过滤，没有 execution 过滤，因此客户端无法可靠关联（不做遍历猜测）。'}
        {' 2) 价格端点 '}
        <code>/api/v1/admin/prices</code>
        {' 仅平台管理员可用（其它角色固定 403），所以组织视角看不到本组织模型的价目，只能由平台管理员在「平台管理 → 价格版本」查看。'}
      </ContractGap>
    </Space>
  );
}

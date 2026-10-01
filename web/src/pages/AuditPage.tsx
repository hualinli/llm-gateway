/**
 * Audit trail (`GET /api/v1/orgs/{org}/audit`) — owner/admin only.
 *
 * A member or billing_viewer receives `403 forbidden`; a non-member receives
 * `404 not_found`. Both are rendered as first-class states instead of an empty
 * table, because "no permission" and "nothing happened" are different facts.
 */

import type { ReactNode } from 'react';
import { Card, Space, Table, Tag, Typography } from 'antd';

import type { AuditEvent } from '../api/types';
import { useAudit } from '../api/queries';
import { DataState } from '../components/states';
import { CursorPagination, useCursorPager } from '../components/CursorPager';
import { ContractGap, PageHeader } from '../components/PageHeader';
import { useActiveOrg } from '../session/SessionProvider';
import { formatTimestamp, shortId } from '../utils/format';

const ACTION_LABELS: Record<string, string> = {
  'org.invitation.create': '邀请创建',
  'org.invitation.accept': '邀请接受',
  'org.member.role_change': '角色变更',
  'org.member.remove': '成员移除',
  'org.owner.transfer': '所有权移交',
  'org.key.create': 'Key 创建',
  'org.key.revoke': 'Key 撤销',
  'admin.user.create': '平台建用户',
  'admin.user.status': '平台改状态',
};

export function AuditPage(): ReactNode {
  const { orgId, role } = useActiveOrg();
  const pager = useCursorPager();
  const audit = useAudit(orgId, pager.cursor, orgId !== null);

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <PageHeader
        title="审计"
        description={`组织 ${orgId ?? '—'} 的 console_audit_events（新→旧）。当前角色：${role ?? '—'}；只有 owner/admin 可以读取。`}
      />

      <Card size="small">
        <DataState
          query={audit}
          loadingRows={5}
          isEmpty={(page) => page.items.length === 0}
          emptyDescription="该组织还没有审计事件"
        >
          {(page) => (
            <>
              <Table<AuditEvent>
                size="small"
                rowKey="id"
                dataSource={page.items}
                pagination={false}
                scroll={{ x: 1100 }}
                expandable={{
                  expandedRowRender: (record) => (
                    <pre className="detail-json">
                      {JSON.stringify(
                        { detail: record.detail, trace_id: record.trace_id, id: record.id },
                        null,
                        2,
                      )}
                    </pre>
                  ),
                  rowExpandable: (record) => record.detail !== null || record.trace_id !== null,
                }}
                columns={[
                  {
                    title: '时间',
                    dataIndex: 'created_at',
                    render: (value: string) => formatTimestamp(value),
                  },
                  {
                    title: '结果',
                    dataIndex: 'outcome',
                    render: (value: string) =>
                      value === 'success' ? (
                        <Tag color="green">success</Tag>
                      ) : (
                        <Tag color="red">failure</Tag>
                      ),
                  },
                  {
                    title: '动作',
                    dataIndex: 'action',
                    render: (value: string) => (
                      <Space size={4}>
                        <Tag>{ACTION_LABELS[value] ?? value}</Tag>
                        <Typography.Text type="secondary">{value}</Typography.Text>
                      </Space>
                    ),
                  },
                  {
                    title: 'actor_user_id',
                    dataIndex: 'actor_user_id',
                    render: (value: string | null) => value ?? '—',
                  },
                  {
                    title: '目标',
                    key: 'target',
                    render: (_, record) => `${record.target_type}:${record.target_id || '—'}`,
                  },
                  {
                    title: 'request_id',
                    dataIndex: 'request_id',
                    render: (value: string | null) =>
                      value ? (
                        <Typography.Text copyable={{ text: value }}>{shortId(value, 14)}</Typography.Text>
                      ) : (
                        '—'
                      ),
                  },
                ]}
              />
              <CursorPagination
                pager={pager}
                hasNext={page.next_cursor !== null}
                loading={audit.isFetching}
                itemCount={page.items.length}
                onNext={() => {
                  if (page.next_cursor) pager.next(page.next_cursor);
                }}
              />
            </>
          )}
        </DataState>
      </Card>

      <ContractGap title="审计没有服务端过滤与时间范围参数">
        契约的 audit 端点只接受 limit/cursor：按动作、结果或时间筛选目前只能在当前页做（会误导），因此本页不提供过滤器。
        审计行也永远不会包含密码、会话/CSRF token 或邀请 token。
      </ContractGap>
    </Space>
  );
}

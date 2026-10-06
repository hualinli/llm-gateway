import type { ReactNode } from 'react';
import { Card, Space, Table, Tag, Typography } from 'antd';

import type { AuditEvent } from '../api/types';
import { useAudit } from '../api/queries';
import { DataState } from '../components/states';
import { CursorPagination, useCursorPager } from '../components/CursorPager';
import { PageHeader } from '../components/PageHeader';
import { useActiveOrg } from '../session/SessionProvider';
import { formatTimestamp, shortId } from '../utils/format';
import { CAN } from '../utils/permissions';

const ACTION_LABELS: Record<string, string> = {
  'org.invitation.create': '已创建邀请',
  'org.invitation.accept': '已接受邀请',
  'org.member.role_change': '已调整成员角色',
  'org.member.remove': '已移除成员',
  'org.member.status': '已暂停或恢复成员',
  'org.member.quota': '已调整成员额度',
  'org.join_request.decision': '已处理加入申请',
  'org.owner.transfer': '已转交企业负责人',
  'org.key.create': '已创建 API Key',
  'org.key.revoke': '已撤销 API Key',
  'admin.user.create': '已创建用户',
  'admin.user.status': '已调整用户状态',
  'admin.price.create': '已发布模型价格',
  'billing.adjustment': '已记录资金变动',
};

const TARGET_LABELS: Record<string, string> = {
  membership: '成员关系',
  invitation: '邀请',
  user: '用户',
  api_key: 'API Key',
  price_version: '模型价格',
  billing_account: '资金账户',
};

export function AuditPage(): ReactNode {
  const { orgId, role } = useActiveOrg();
  const pager = useCursorPager();
  const canRead = CAN.manageMembers(role);
  const audit = useAudit(orgId, pager.cursor, canRead && orgId !== null);

  if (!canRead) {
    return <Card><Typography.Text>操作记录仅对企业负责人和管理员开放。</Typography.Text></Card>;
  }

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <PageHeader title="操作记录" description="查看近期邀请、成员、Key 和资金操作记录。" />
      <Card>
        <DataState query={audit} loadingRows={5} isEmpty={(page) => page.items.length === 0} emptyDescription="暂时没有需要展示的操作记录。">
          {(page) => <>
            <Table<AuditEvent>
              size="small"
              rowKey="id"
              dataSource={page.items}
              pagination={false}
              scroll={{ x: 800 }}
              columns={[
                { title: '时间', dataIndex: 'created_at', render: (value: string) => formatTimestamp(value) },
                { title: '结果', dataIndex: 'outcome', render: (value: string) => <Tag color={value === 'success' ? 'green' : 'red'}>{value === 'success' ? '已完成' : '未完成'}</Tag> },
                { title: '操作', dataIndex: 'action', render: (value: string) => ACTION_LABELS[value] ?? '其他账户操作' },
                { title: '操作者', dataIndex: 'actor_user_id', render: (value: string | null) => value ? `成员 ${shortId(value)}` : '系统' },
                { title: '相关对象', dataIndex: 'target_type', render: (value: string, row) => <Space><Typography.Text>{TARGET_LABELS[value] ?? '账户对象'}</Typography.Text><Typography.Text type="secondary">{shortId(row.target_id)}</Typography.Text></Space> },
              ]}
            />
            <CursorPagination pager={pager} hasNext={page.next_cursor !== null} loading={audit.isFetching} itemCount={page.items.length} onNext={() => page.next_cursor && pager.next(page.next_cursor)} />
          </>}
        </DataState>
      </Card>
    </Space>
  );
}

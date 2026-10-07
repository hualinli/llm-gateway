import { useMemo, useState, type ReactNode } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import {
  Alert,
  App as AntdApp,
  Button,
  Card,
  Form,
  Input,
  Modal,
  Radio,
  Result,
  Select,
  Space,
  Table,
  Tag,
  Typography,
} from 'antd';
import { CopyOutlined, PlusOutlined } from '@ant-design/icons';

import { consoleApi } from '../api/client';
import type { Invitation, JoinRequest, Member, OrgRole } from '../api/types';
import {
  queryKeys,
  useChangeMemberRole,
  useCreateInvitation,
  useDecideJoinRequest,
  useInvitations,
  useMembers,
  useMemberUsageSummary,
  useOrgJoinRequests,
  useRemoveMember,
  useSetMemberQuota,
  useTransferOwnership,
} from '../api/queries';
import { MicroYuan } from '../components/Money';
import { PageHelp } from '../components/PageHelp';
import { PageHeader } from '../components/PageHeader';
import { DataState, ErrorView } from '../components/states';
import { useActiveOrg, useSession } from '../session/SessionProvider';
import { beijingMonthOf, formatTimestamp, microToYuanInput, yuanToMicro } from '../utils/format';
import { assignableRoles, CAN, ROLE_LABELS } from '../utils/permissions';

interface QuotaForm { limit_mode: 'unlimited' | 'limited'; amount_yuan?: string }
interface InviteForm extends QuotaForm { email: string; role: 'member' | 'admin' }
interface RoleForm extends QuotaForm { role: 'member' | 'admin' }

function quotaLabel(limit: string | null | undefined): ReactNode {
  if (limit === null || limit === undefined) return <Tag color="blue">不限额</Tag>;
  if (limit === '0') return <Tag color="red">禁止消费</Tag>;
  return <><MicroYuan value={limit} /> / 月</>;
}

function amountToLimit(form: QuotaForm): string | null {
  if (form.limit_mode === 'unlimited') return null;
  return yuanToMicro(form.amount_yuan ?? '');
}

export function MembersPage(): ReactNode {
  const { orgId, membership, role } = useActiveOrg();
  const { me } = useSession();
  const { message, modal } = AntdApp.useApp();
  const client = useQueryClient();
  const [quotaForm] = Form.useForm<QuotaForm>();
  const [inviteForm] = Form.useForm<InviteForm>();
  const [decisionForm] = Form.useForm<QuotaForm>();
  const [roleForm] = Form.useForm<RoleForm>();
  const [quotaTarget, setQuotaTarget] = useState<Member | null>(null);
  const [roleTarget, setRoleTarget] = useState<Member | null>(null);
  const [inviteOpen, setInviteOpen] = useState(false);
  const [reviewTarget, setReviewTarget] = useState<JoinRequest | null>(null);
  const [createdInvite, setCreatedInvite] = useState<{ email: string; url: string } | null>(null);
  const [pageError, setPageError] = useState<unknown>(null);
  const month = useMemo(() => beijingMonthOf(), []);
  const canManage = membership?.organization.type === 'enterprise' && CAN.manageMembers(role);

  const members = useMembers(orgId, canManage);
  const invitations = useInvitations(orgId, canManage);
  const joinRequests = useOrgJoinRequests(orgId, canManage);
  const usage = useMemberUsageSummary(orgId, month, canManage);
  const createInvitation = useCreateInvitation(orgId ?? '');
  const removeMember = useRemoveMember(orgId ?? '');
  const changeMemberRole = useChangeMemberRole(orgId ?? '');
  const setQuota = useSetMemberQuota(orgId ?? '');
  const decideJoinRequest = useDecideJoinRequest(orgId ?? '');
  const transferOwnership = useTransferOwnership(orgId ?? '');
  const setMemberStatus = useMutation({
    mutationFn: ({ user, status }: { user: string; status: 'active' | 'suspended' }) =>
      consoleApi.changeMemberRole(orgId ?? '', user, { status }),
    onSuccess: async () => {
      await client.invalidateQueries({ queryKey: queryKeys.members(orgId ?? '') });
      await client.invalidateQueries({ queryKey: ['org', orgId, 'audit'] });
    },
  });

  if (!membership || membership.organization.type === 'personal') {
    return <Result status="info" title="个人账户不设团队成员" subTitle="个人账户只由你本人使用；需要团队协作时，请申请加入或开通企业账户。" />;
  }
  if (!canManage) {
    return <Result status="warning" title="成员管理仅对企业负责人和管理员开放" subTitle="你仍可在自己的账户中查看消费、额度和 API Key。" />;
  }

  const openQuota = (member: Member): void => {
    setQuotaTarget(member);
    quotaForm.setFieldsValue({
      limit_mode: member.monthly_limit_micro === null || member.monthly_limit_micro === undefined ? 'unlimited' : 'limited',
      amount_yuan: microToYuanInput(member.monthly_limit_micro),
    });
  };

  const openRoleEditor = (member: Member): void => {
    setRoleTarget(member);
    roleForm.resetFields();
    roleForm.setFieldsValue({ role: member.role === 'admin' ? 'member' : 'admin' });
  };

  const saveRole = async (): Promise<void> => {
    if (!roleTarget) return;
    const values = await roleForm.validateFields();
    const body = values.role === 'member'
      ? { role: 'member' as const, monthly_limit_micro: amountToLimit(values) }
      : { role: 'admin' as const };
    try {
      await changeMemberRole.mutateAsync({ user: roleTarget.user_id, role: body });
      message.success(values.role === 'member' ? '成员角色和月度额度已更新。' : '成员已成为管理员。');
      setRoleTarget(null);
    } catch (cause) { setPageError(cause); }
  };

  const saveQuota = async (): Promise<void> => {
    if (!quotaTarget) return;
    const values = await quotaForm.validateFields();
    const limit = amountToLimit(values);
    if (limit === null && values.limit_mode !== 'unlimited') return;
    try {
      await setQuota.mutateAsync({ user: quotaTarget.user_id, monthlyLimitMicro: limit });
      message.success('月度额度已更新。已发生消费不受调整影响。');
      setQuotaTarget(null);
      await client.invalidateQueries({ queryKey: ['org', orgId, 'member-summary', month] });
    } catch (cause) { setPageError(cause); }
  };

  const saveInvite = async (): Promise<void> => {
    const values = await inviteForm.validateFields();
    const monthlyLimit = amountToLimit(values);
    if (values.role === 'member' && monthlyLimit === null && values.limit_mode !== 'unlimited') return;
    try {
      const response = await createInvitation.mutateAsync({
        email: values.email.trim(),
        role: values.role as OrgRole,
        ...(values.role === 'member' ? { monthly_limit_micro: monthlyLimit } : {}),
        expires_in_seconds: 259200,
      });
      setCreatedInvite({ email: values.email.trim(), url: `${window.location.origin}/invitations/${encodeURIComponent(response.token)}` });
      setInviteOpen(false);
      inviteForm.resetFields();
    } catch (cause) { setPageError(cause); }
  };

  const decide = async (request: JoinRequest, status: 'approved' | 'rejected'): Promise<void> => {
    if (status === 'rejected') {
      try {
        await decideJoinRequest.mutateAsync({ requestId: request.id, body: { status, monthly_limit_micro: null } });
        message.success('申请已处理。');
        setReviewTarget(null);
      } catch (cause) { setPageError(cause); }
      return;
    }
    setReviewTarget(request);
    decisionForm.resetFields();
  };

  const approve = async (): Promise<void> => {
    if (!reviewTarget) return;
    const values = await decisionForm.validateFields();
    const monthlyLimit = amountToLimit(values);
    if (monthlyLimit === null && values.limit_mode !== 'unlimited') return;
    try {
      await decideJoinRequest.mutateAsync({ requestId: reviewTarget.id, body: { status: 'approved', monthly_limit_micro: monthlyLimit } });
      message.success('申请已通过，成员额度已设置。');
      setReviewTarget(null);
    } catch (cause) { setPageError(cause); }
  };

  const askRemove = (member: Member): void => {
    modal.confirm({
      title: `移除 ${member.email}？`,
      content: '对方将无法再使用此企业账户的 API Key，历史消费记录仍会保留。',
      okText: '移除成员',
      okButtonProps: { danger: true },
      cancelText: '取消',
      onOk: async () => {
        await removeMember.mutateAsync(member.user_id);
        message.success('成员已移除。');
      },
    });
  };

  const askTransfer = (member: Member): void => {
    modal.confirm({
      title: `将企业负责人转交给 ${member.email}？`,
      content: '转交后，对方成为负责人，你将成为管理员。',
      okText: '确认转交',
      cancelText: '取消',
      onOk: async () => {
        await transferOwnership.mutateAsync(member.user_id);
        message.success('负责人已变更。');
        await client.invalidateQueries({ queryKey: queryKeys.me });
      },
    });
  };

  const usageRows = usage.data?.items ?? [];
  const assignable = assignableRoles(role);
  return (
    <Space className="responsive-page-content" direction="vertical" size={18} style={{ width: '100%', minWidth: 0 }}>
      <PageHeader title="成员与申请" description="管理加入申请、成员角色和月度额度。每位成员只管理自己的 API Key。" extra={<Button icon={<PlusOutlined />} type="primary" onClick={() => { inviteForm.resetFields(); setInviteOpen(true); }}>邀请成员</Button>} />
      <PageHelp title="成员额度如何计算？">降低额度会影响新的调用，已开始的请求继续结算。0 元表示禁止新消费；不限额表示直接使用企业余额。额度按北京时间自然月重新计算。</PageHelp>
      {pageError ? <ErrorView error={pageError} title="操作未完成" /> : null}
      <Card className="responsive-table-card" title={`成员消费 · ${month}`} data-testid="member-spend-summary">
        <DataState query={usage} emptyDescription="本月还没有成员消费记录。">
          {(summary) => <Table
            rowKey="user_id"
            size="small"
            pagination={false}
            scroll={{ x: 1050 }}
            dataSource={summary.items}
            columns={[
              { title: '成员', dataIndex: 'email', width: 240 },
              { title: '已结算调用', dataIndex: 'settled_count', width: 120 },
              { title: '处理中', dataIndex: 'pending_count', width: 110 },
              { title: '需要核对', dataIndex: 'review_required_count', width: 120 },
              { title: 'Token 用量', dataIndex: 'total_tokens', width: 130, render: (value: number) => value.toLocaleString('en-US') },
              { title: '已结算费用', dataIndex: 'sales_micro', width: 150, render: (value: string) => <MicroYuan value={value} /> },
              { title: '用量待确认', dataIndex: 'unknown_usage_count', width: 130 },
            ]}
          />}
        </DataState>
      </Card>
      <Card className="responsive-table-card" title="企业成员" data-testid="members-list">
        <DataState query={members} emptyDescription="还没有成员。可以发送邀请或审批加入申请。">
          {(page) => <Table<Member>
            rowKey="user_id"
            size="middle"
            pagination={{ pageSize: 10 }}
            scroll={{ x: 1120 }}
            dataSource={page.items}
            columns={[
              { title: '成员', dataIndex: 'email', width: 280, render: (email: string, row) => <Space direction="vertical" size={0}><Typography.Text strong>{email}</Typography.Text><Typography.Text type="secondary">{row.role === 'owner' ? '企业负责人' : ROLE_LABELS[row.role]}</Typography.Text></Space> },
              { title: '状态', dataIndex: 'status', width: 120, render: (status: Member['status']) => <Tag color={status === 'active' ? 'green' : 'orange'}>{status === 'active' ? '正常' : '已暂停'}</Tag> },
              { title: '月度额度', dataIndex: 'monthly_limit_micro', width: 180, render: (value: string | null | undefined) => quotaLabel(value) },
              { title: '本月已结算', key: 'spent', width: 180, render: (_, row) => { const record = usageRows.find((item) => item.user_id === row.user_id); return record ? <MicroYuan value={record.sales_micro} /> : '—'; } },
              { title: '操作', key: 'actions', render: (_, row) => {
                const isSelf = row.user_id === me?.user.id;
                const canManageTarget = role === 'owner' ? row.role !== 'owner' : row.role === 'member' && !isSelf;
                const canSetCap = role === 'owner' || (row.role === 'member' && !isSelf);
                return <Space wrap>
                  {canSetCap ? <Button size="small" onClick={() => openQuota(row)}>调整额度</Button> : null}
                  {role === 'owner' && !isSelf && row.role !== 'owner' ? (
                    <>
                      <Button size="small" onClick={() => openRoleEditor(row)}>{row.role === 'admin' ? '降为成员' : '设为管理员'}</Button>
                      {row.role === 'admin' ? <Button size="small" onClick={() => askTransfer(row)}>转交负责人</Button> : null}
                    </>
                  ) : null}
                  {canManageTarget ? <Button size="small" onClick={() => setMemberStatus.mutate({ user: row.user_id, status: row.status === 'active' ? 'suspended' : 'active' }, { onSuccess: () => message.success(row.status === 'active' ? '成员已暂停。' : '成员已恢复。'), onError: (error) => setPageError(error) })}>{row.status === 'active' ? '暂停成员' : '恢复成员'}</Button> : null}
                  {canManageTarget ? <Button danger size="small" onClick={() => askRemove(row)}>移除</Button> : null}
                </Space>;
              } },
            ]}
          />}
        </DataState>
      </Card>
      <Card className="responsive-table-card" title="加入申请" data-testid="join-requests">
        <DataState query={joinRequests} emptyDescription="暂时没有待审批的申请。">
          {(page) => <Table<JoinRequest>
            rowKey="id"
            size="middle"
            pagination={false}
            scroll={{ x: 820 }}
            dataSource={page.pages.flatMap((item) => item.items)}
            locale={{ emptyText: '暂时没有待审批的申请。' }}
            columns={[
              { title: '申请人', dataIndex: 'email', render: (email: string, row) => <Space direction="vertical" size={0}><Typography.Text strong>{row.display_name}</Typography.Text><Typography.Text type="secondary">{email}</Typography.Text></Space> },
              { title: '申请时间', dataIndex: 'created_at', render: (value: string) => formatTimestamp(value) },
              { title: '操作', key: 'actions', render: (_, row) => <Space><Button size="small" type="primary" onClick={() => void decide(row, 'approved')}>通过并设额度</Button><Button size="small" danger onClick={() => void decide(row, 'rejected')}>拒绝</Button></Space> },
            ]}
          />}
        </DataState>
        {joinRequests.data && joinRequests.hasNextPage ? <Button
          size="small"
          loading={joinRequests.isFetchingNextPage}
          onClick={() => void joinRequests.fetchNextPage()}
        >加载更多待审批申请（已载入 {joinRequests.data.pages.reduce((count, page) => count + page.items.length, 0)} 条）</Button> : null}
      </Card>
      <Card className="responsive-table-card" title="待接受邀请" data-testid="member-invitations">
        <DataState query={invitations} emptyDescription="没有待接受的邀请。">
          {(page) => <Table<Invitation>
            rowKey="id"
            size="small"
            pagination={false}
            scroll={{ x: 680 }}
            dataSource={page.items.filter((item) => item.status === 'pending')}
            columns={[
              { title: '受邀邮箱', dataIndex: 'email' },
              { title: '角色', dataIndex: 'role', render: (value: OrgRole) => ROLE_LABELS[value] },
              { title: '有效期至', dataIndex: 'expires_at', render: (value: string) => formatTimestamp(value) },
            ]}
          />}
        </DataState>
      </Card>

      <Modal title={`调整 ${quotaTarget?.email ?? '成员'} 的月度额度`} open={quotaTarget !== null} onCancel={() => setQuotaTarget(null)} onOk={() => void saveQuota()} confirmLoading={setQuota.isPending} okText="保存额度" cancelText="取消" destroyOnHidden>
        <Form form={quotaForm} layout="vertical"><QuotaFields /></Form>
        <Typography.Paragraph type="secondary">当前额度：{quotaLabel(quotaTarget?.monthly_limit_micro)}</Typography.Paragraph>
        {setQuota.isError ? <ErrorView error={setQuota.error} title="额度未更新" /> : null}
      </Modal>

      <Modal title={`调整 ${roleTarget?.email ?? '成员'} 的角色`} open={roleTarget !== null} onCancel={() => setRoleTarget(null)} onOk={() => void saveRole()} confirmLoading={changeMemberRole.isPending} okText="保存变更" cancelText="取消" destroyOnHidden>
        <Form form={roleForm} layout="vertical">
          <Form.Item name="role" label="企业角色" rules={[{ required: true }]}>
            <Select options={[{ value: 'admin', label: '管理员' }, { value: 'member', label: '成员' }]} />
          </Form.Item>
          <Form.Item noStyle shouldUpdate={(previous: RoleForm, current: RoleForm) => previous.role !== current.role}>
            {({ getFieldValue }) => getFieldValue('role') === 'member' ? <QuotaFields compact /> : null}
          </Form.Item>
        </Form>
        {changeMemberRole.isError ? <ErrorView error={changeMemberRole.error} title="角色未更新" /> : null}
      </Modal>

      <Modal title="邀请成员" open={inviteOpen} onCancel={() => setInviteOpen(false)} onOk={() => void saveInvite()} confirmLoading={createInvitation.isPending} okText="生成邀请链接" cancelText="取消" destroyOnHidden>
        <Form form={inviteForm} layout="vertical" initialValues={{ role: 'member' }}>
          <Form.Item name="email" label="对方邮箱" rules={[{ required: true, type: 'email', message: '请输入有效邮箱' }]}><Input autoComplete="email" /></Form.Item>
          <Form.Item name="role" label="加入后的角色" rules={[{ required: true }]}>
            <Select options={assignable.map((value) => ({ value, label: ROLE_LABELS[value] }))} />
          </Form.Item>
          <Form.Item noStyle shouldUpdate={(previous: InviteForm, current: InviteForm) => previous.role !== current.role}>
            {({ getFieldValue }) => getFieldValue('role') === 'member' ? <QuotaFields compact /> : null}
          </Form.Item>
          <Typography.Text type="secondary">邀请链接 72 小时内有效。普通成员需要明确设置月度额度。</Typography.Text>
        </Form>
        {createInvitation.isError ? <div style={{ marginTop: 12 }}><ErrorView error={createInvitation.error} title="邀请未创建" /></div> : null}
      </Modal>

      <Modal title="批准加入申请" open={reviewTarget !== null} onCancel={() => setReviewTarget(null)} onOk={() => void approve()} confirmLoading={decideJoinRequest.isPending} okText="通过并加入企业" cancelText="取消" destroyOnHidden>
        <Space direction="vertical" style={{ width: '100%' }}>
          <Typography.Text>{reviewTarget?.display_name} · {reviewTarget?.email}</Typography.Text>
          <Alert type="info" showIcon message="通过申请时必须明确设置成员额度。" />
          <Form form={decisionForm} layout="vertical"><QuotaFields compact /></Form>
          {decideJoinRequest.isError ? <ErrorView error={decideJoinRequest.error} title="申请未处理" /> : null}
        </Space>
      </Modal>

      <Modal title="邀请链接已生成" open={createdInvite !== null} onCancel={() => setCreatedInvite(null)} footer={<Button type="primary" onClick={() => setCreatedInvite(null)}>完成</Button>} destroyOnHidden>
        {createdInvite ? <Space direction="vertical" style={{ width: '100%' }}>
          <Alert type="warning" showIcon message="链接只在这里显示，请复制后交给对方。" />
          <Typography.Text>{createdInvite.email}</Typography.Text>
          <Input readOnly value={createdInvite.url} aria-label="邀请链接" />
          <Button icon={<CopyOutlined />} onClick={async () => { try { await navigator.clipboard.writeText(createdInvite.url); message.success('已复制邀请链接'); } catch { message.error('复制失败，请手动复制链接。'); } }}>复制邀请链接</Button>
        </Space> : null}
      </Modal>
    </Space>
  );
}

function QuotaFields({ compact = false }: { compact?: boolean }): ReactNode {
  return (
    <>
      <Form.Item name="limit_mode" label="月度上限" rules={[{ required: true, message: '请选择月度上限' }]}>
        <Radio.Group options={[{ value: 'limited', label: '设置金额' }, { value: 'unlimited', label: '不限额' }]} />
      </Form.Item>
      <Form.Item noStyle shouldUpdate={(previous: QuotaForm, current: QuotaForm) => previous.limit_mode !== current.limit_mode}>
        {({ getFieldValue }) => getFieldValue('limit_mode') === 'limited' ? (
          <Form.Item name="amount_yuan" label={compact ? '额度（元 / 月）' : '额度（元）'} rules={[
            { required: true, message: '请输入额度；0 表示暂停新消费' },
            { validator: (_, value: string | undefined) => value !== undefined && yuanToMicro(value) !== null ? Promise.resolve() : Promise.reject(new Error('请输入最多 6 位小数的非负金额')) },
          ]}>
            <Input inputMode="decimal" placeholder="例如 500" addonAfter="元" />
          </Form.Item>
        ) : null}
      </Form.Item>
      <Typography.Text type="secondary">额度按北京时间自然月计算，未使用部分不结转；降低额度不会取消已受理请求。</Typography.Text>
    </>
  );
}

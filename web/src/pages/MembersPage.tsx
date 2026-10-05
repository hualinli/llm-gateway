/**
 * Members and invitations (admin/owner surface, read-only for billing_viewer).
 *
 * Refusals are shown with their T01A code and request id — the interesting ones
 * are `409 conflict` ("organization must keep at least one owner") and
 * `403 forbidden` (an admin may not touch an owner or another admin, and
 * `owner` can only be granted through the ownership-transfer endpoint).
 */

import { useMemo, useState, type ReactNode } from 'react';
import {
  Alert,
  App as AntdApp,
  Button,
  Card,
  Drawer,
  Form,
  Input,
  InputNumber,
  Modal,
  Popconfirm,
  Select,
  Space,
  Table,
  Tag,
  Typography,
} from 'antd';
import { BarChartOutlined, PlusOutlined, SwapOutlined } from '@ant-design/icons';

import {
  useChangeMemberRole,
  useCreateInvitation,
  useInvitations,
  useMembers,
  useRemoveMember,
  useTransferOwnership,
  useUsage,
} from '../api/queries';
import type { CreateInvitationResponse, Invitation, Member, OrgRole, UsageEvent } from '../api/types';
import { DataState, ErrorView, errorSummary } from '../components/states';
import { ContractGap, PageHeader } from '../components/PageHeader';
import { useActiveOrg } from '../session/SessionProvider';
import { formatCostCents, formatNumber, formatTimestamp, shortId } from '../utils/format';
import { assignableRoles, CAN, ROLE_LABELS, ROLE_TAG_COLOR } from '../utils/permissions';

interface InviteForm {
  email: string;
  role: OrgRole;
  expires_in_hours: number;
}


export function MembersPage(): ReactNode {
  const { orgId, role } = useActiveOrg();
  const { message } = AntdApp.useApp();

  const members = useMembers(orgId, orgId !== null);
  const invitations = useInvitations(orgId, CAN.manageMembers(role) && orgId !== null);
  const createInvitation = useCreateInvitation(orgId ?? '');
  const changeRoleMutation = useChangeMemberRole(orgId ?? '');
  const removeMember = useRemoveMember(orgId ?? '');
  const transferOwnership = useTransferOwnership(orgId ?? '');

  const [inviteOpen, setInviteOpen] = useState(false);
  const [inviteForm] = Form.useForm<InviteForm>();
  const [createdInvite, setCreatedInvite] = useState<CreateInvitationResponse | null>(null);
  const [transferTarget, setTransferTarget] = useState<Member | null>(null);
  const [transferConfirm, setTransferConfirm] = useState('');
  const [usageMember, setUsageMember] = useState<Member | null>(null);

  const canManage = CAN.manageMembers(role);
  const invitableRoles: OrgRole[] = CAN.createAdmin(role)
    ? ['member', 'billing_viewer', 'admin', 'owner']
    : ['member', 'billing_viewer'];

  const submitInvite = async (): Promise<void> => {
    const values = await inviteForm.validateFields();
    try {
      const response = await createInvitation.mutateAsync({
        email: values.email.trim(),
        role: values.role,
        expires_in_seconds: Math.round(values.expires_in_hours * 3600),
      });
      setInviteOpen(false);
      inviteForm.resetFields();
      setCreatedInvite(response);
    } catch {
      // Rendered inline from `createInvitation.error`.
    }
  };

  const changeRole = async (member: Member, nextRole: OrgRole): Promise<void> => {
    try {
      await changeRoleMutation.mutateAsync({ user: member.user_id, role: nextRole });
      message.success(`${member.email} 的角色已改为 ${ROLE_LABELS[nextRole]}`);
    } catch (error) {
      message.error(`角色变更失败 — ${errorSummary(error)}`);
    }
  };

  const remove = async (member: Member): Promise<void> => {
    try {
      await removeMember.mutateAsync(member.user_id);
      message.success(`已移除 ${member.email}`);
    } catch (error) {
      message.error(`移除失败 — ${errorSummary(error)}`);
    }
  };

  const doTransfer = async (): Promise<void> => {
    if (!transferTarget) return;
    try {
      const result = await transferOwnership.mutateAsync(transferTarget.user_id);
      message.success(
        `所有权已转移：新 owner user_id=${result.owner_user_id}，你已成为 ${result.previous_owner_role}`,
      );
      setTransferTarget(null);
      setTransferConfirm('');
    } catch (error) {
      message.error(`转移失败 — ${errorSummary(error)}`);
    }
  };

  const inviteLink = createdInvite
    ? `${window.location.origin}/invitations/${createdInvite.token}`
    : '';

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <PageHeader
        title="成员与邀请"
        description="角色来自服务端 memberships；界面按钮只反映权限矩阵，服务端始终是唯一执行点。"
        extra={
          canManage ? (
            <Button
              type="primary"
              icon={<PlusOutlined />}
              onClick={() => {
                createInvitation.reset();
                inviteForm.setFieldsValue({ role: 'member', expires_in_hours: 72 });
                setInviteOpen(true);
              }}
            >
              邀请成员
            </Button>
          ) : (
            <Tag>只读（billing_viewer）</Tag>
          )
        }
      />

      {role === 'member' ? (
        <Alert
          type="info"
          showIcon
          message="member 只能看到自己"
          description="GET /orgs/{org}/members 对普通成员返回只包含本人的单元素列表：他们可以确认自己的角色，但不能枚举同事。"
        />
      ) : null}

      <Card size="small" title="成员">
        <DataState
          query={members}
          loadingRows={4}
          isEmpty={(page) => page.items.length === 0}
          emptyDescription="没有可见成员"
        >
          {(page) => (
            <Table<Member>
              size="small"
              rowKey="user_id"
              dataSource={page.items}
              pagination={false}
              scroll={{ x: 860 }}
              columns={[
                { title: '邮箱', dataIndex: 'email' },
                { title: 'user_id', dataIndex: 'user_id' },
                {
                  title: '角色',
                  dataIndex: 'role',
                  render: (value: OrgRole, record) => {
                    const options = assignableRoles(role);
                    const editable =
                      canManage &&
                      record.user_id !== undefined &&
                      options.length > 0 &&
                      !(
                        role === 'admin' &&
                        (record.role === 'owner' || record.role === 'admin')
                      );
                    if (!editable) {
                      return <Tag color={ROLE_TAG_COLOR[value]}>{ROLE_LABELS[value]}</Tag>;
                    }
                    return (
                      <Space size={4} wrap>
                        <Select<OrgRole>
                          size="small"
                          style={{ minWidth: 150 }}
                          value={value}
                          aria-label={`修改 ${record.email} 的角色`}
                          loading={changeRoleMutation.isPending}
                          onChange={(nextRole) => {
                            if (nextRole !== value) void changeRole(record, nextRole);
                          }}
                          options={options.map((option) => ({
                            value: option,
                            label: ROLE_LABELS[option],
                          }))}
                        />
                        <Tag color={ROLE_TAG_COLOR[value]}>{ROLE_LABELS[value]}</Tag>
                      </Space>
                    );
                  },
                },
                {
                  title: '加入时间',
                  dataIndex: 'created_at',
                  render: (value: string) => formatTimestamp(value),
                },
                {
                  title: '操作',
                  key: 'action',
                  render: (_, record) => (
                    <Space size={4} wrap>
                      {CAN.viewOrgUsage(role) ? (
                        <Button
                          size="small"
                          icon={<BarChartOutlined />}
                          aria-label={`查看 ${record.email} 的用量`}
                          onClick={() => setUsageMember(record)}
                        >
                          用量
                        </Button>
                      ) : null}
                      {CAN.transferOwner(role) && record.role !== 'owner' ? (
                        <Button
                          size="small"
                          icon={<SwapOutlined />}
                          onClick={() => {
                            setTransferTarget(record);
                            setTransferConfirm('');
                          }}
                        >
                          移交所有权
                        </Button>
                      ) : null}
                      {canManage &&
                      !(role === 'admin' && (record.role === 'owner' || record.role === 'admin')) ? (
                        <Popconfirm
                          title="移除这位成员？"
                          description={
                            <Space direction="vertical" size={2}>
                              <span>
                                邮箱：<strong>{record.email}</strong>
                              </span>
                              <span>user_id：{record.user_id}</span>
                              <span>当前角色：{ROLE_LABELS[record.role]}</span>
                              <span>移除是软删除；最后一位 owner 会被服务端拒绝（409）。</span>
                            </Space>
                          }
                          okText="移除"
                          okButtonProps={{ danger: true }}
                          cancelText="取消"
                          onConfirm={() => remove(record)}
                        >
                          <Button size="small" danger>
                            移除
                          </Button>
                        </Popconfirm>
                      ) : null}
                    </Space>
                  ),
                },
              ]}
            />
          )}
        </DataState>
      </Card>

      {CAN.manageMembers(role) ? (
        <Card size="small" title="邀请（token 只返回一次）">
          <DataState
            query={invitations}
            loadingRows={3}
            isEmpty={(page) => page.items.length === 0}
            emptyDescription="还没有邀请记录"
          >
            {(page) => (
              <Table<Invitation>
                size="small"
                rowKey="id"
                dataSource={page.items}
                pagination={false}
                scroll={{ x: 860 }}
                columns={[
                  { title: '邮箱（固定，接受时校验）', dataIndex: 'email' },
                  {
                    title: '角色',
                    dataIndex: 'role',
                    render: (value: OrgRole) => (
                      <Tag color={ROLE_TAG_COLOR[value]}>{ROLE_LABELS[value]}</Tag>
                    ),
                  },
                  {
                    title: '状态',
                    dataIndex: 'status',
                    render: (value: string) => {
                      const color =
                        value === 'pending' ? 'blue' : value === 'accepted' ? 'green' : 'red';
                      return <Tag color={color}>{value}</Tag>;
                    },
                  },
                  {
                    title: '过期时间',
                    dataIndex: 'expires_at',
                    render: (value: string) => formatTimestamp(value),
                  },
                  {
                    title: '创建时间',
                    dataIndex: 'created_at',
                    render: (value: string) => formatTimestamp(value),
                  },
                  {
                    title: '接受时间',
                    dataIndex: 'accepted_at',
                    render: (value: string | null) => (value ? formatTimestamp(value) : '—'),
                  },
                ]}
              />
            )}
          </DataState>
        </Card>
      ) : null}

      <ContractGap title="没有撤销邀请（revoke invitation）端点">
        契约只有创建与列出邀请：已发出的邀请在过期或被接受前无法通过 API 撤销，列表里也永远不会出现 token。
      </ContractGap>

      <MemberUsageDrawer
        orgId={orgId}
        member={usageMember}
        onClose={() => setUsageMember(null)}
      />

      {/* --------------------------------------------------- invite modal -- */}
      <Modal
        title="邀请成员"
        open={inviteOpen}
        onCancel={() => setInviteOpen(false)}
        onOk={() => {
          void submitInvite();
        }}
        okText="生成邀请链接"
        cancelText="取消"
        confirmLoading={createInvitation.isPending}
        destroyOnHidden
      >
        <Form<InviteForm> form={inviteForm} layout="vertical">
          <Form.Item
            name="email"
            label="被邀请人邮箱"
            rules={[{ required: true, type: 'email', message: '请输入合法邮箱' }]}
            extra="邮箱在创建时固定、接受时校验；不发送邮件，也不通过投递验证地址。"
          >
            <Input autoFocus placeholder="invitee@example.com" />
          </Form.Item>
          <Form.Item name="role" label="角色" rules={[{ required: true }]}>
            <Select
              options={invitableRoles.map((option) => ({
                value: option,
                label: ROLE_LABELS[option],
              }))}
            />
          </Form.Item>
          <Form.Item
            name="expires_in_hours"
            label="有效期（小时）"
            rules={[{ required: true }]}
            extra="服务端取值范围 60 秒 ~ 30 天，默认 72 小时。"
          >
            <InputNumber min={1} max={720} style={{ width: '100%' }} />
          </Form.Item>
        </Form>
        {createInvitation.isError ? (
          <ErrorView error={createInvitation.error} title="创建邀请失败" />
        ) : null}
      </Modal>

      {/* -------------------------------------------------- link once modal */}
      <Modal
        title="邀请链接（只显示这一次）"
        open={createdInvite !== null}
        onCancel={() => setCreatedInvite(null)}
        footer={
          <Button type="primary" onClick={() => setCreatedInvite(null)}>
            我已复制链接
          </Button>
        }
        destroyOnHidden
      >
        {createdInvite ? (
          <Space direction="vertical" size={12} style={{ width: '100%' }}>
            <Alert
              type="warning"
              showIcon
              message="服务端只保存 token 的 SHA-256"
              description="这个链接离开本对话框后无法再次获取；丢失只能重新发起邀请。链接本身就是凭据，不要贴到公开渠道。"
            />
            <div className="secret-block">
              <Typography.Text copyable={{ text: inviteLink }} code>
                {inviteLink}
              </Typography.Text>
            </div>
            <Space direction="vertical" size={2}>
              <Typography.Text>邮箱：{createdInvite.invitation.email}</Typography.Text>
              <Typography.Text>
                角色：{ROLE_LABELS[createdInvite.invitation.role]}
              </Typography.Text>
              <Typography.Text>
                过期：{formatTimestamp(createdInvite.invitation.expires_at)}
              </Typography.Text>
              <Typography.Text type="secondary">
                被邀请人需要用一个邮箱匹配的账号登录后打开该链接（POST
                /api/v1/invitations/&#123;token&#125;/accept）。
              </Typography.Text>
            </Space>
          </Space>
        ) : null}
      </Modal>

      {/* ------------------------------------------------ transfer modal -- */}
      <Modal
        title="移交所有权"
        open={transferTarget !== null}
        onCancel={() => {
          setTransferTarget(null);
          setTransferConfirm('');
        }}
        onOk={() => {
          void doTransfer();
        }}
        okText="确认移交"
        okButtonProps={{ danger: true, disabled: transferConfirm !== transferTarget?.email }}
        cancelText="取消"
        confirmLoading={transferOwnership.isPending}
      >
        <Space direction="vertical" size={12} style={{ width: '100%' }}>
          <Alert
            type="warning"
            showIcon
            message="这是唯一的 owner 授予路径"
            description="移交成功后目标成员成为 owner，你会降级为 admin；组织始终保留至少一位 owner。PATCH /members/{user} 不能授予 owner（403）。"
          />
          <Typography.Text>
            目标：<strong>{transferTarget?.email}</strong> · user_id={transferTarget?.user_id}
          </Typography.Text>
          <Input
            placeholder={`输入 ${transferTarget?.email ?? ''} 以确认`}
            value={transferConfirm}
            onChange={(event) => setTransferConfirm(event.target.value.trim())}
            aria-label="确认移交邮箱"
          />
          {transferOwnership.isError ? (
            <ErrorView error={transferOwnership.error} title="移交失败" />
          ) : null}
        </Space>
      </Modal>
    </Space>
  );
}

/* ------------------------------------------------- per-member breakdown */

/**
 * One member's real usage, read through the server-side `user` filter of
 * `GET /orgs/{org}/usage`. Nothing is aggregated client-side: the totals are the
 * server's totals for the filtered window, and the window is stated on screen.
 *
 * This is the T08 "成员分解" deliverable. The contract has no group-by endpoint,
 * so the breakdown is one member at a time, by design.
 */
function MemberUsageDrawer({
  orgId,
  member,
  onClose,
}: {
  orgId: string | null;
  member: Member | null;
  onClose: () => void;
}): ReactNode {
  // A fixed 7-day window, recomputed when the drawer target changes so the
  // query key stays stable while the drawer is open.
  const query = useMemo(() => {
    const to = new Date();
    const from = new Date(to.getTime() - 7 * 24 * 60 * 60 * 1000);
    return { from: from.toISOString(), to: to.toISOString(), limit: 20 };
  }, [member?.user_id]);

  const usage = useUsage(
    orgId,
    { ...query, ...(member ? { user: member.user_id } : {}) },
    orgId !== null && member !== null,
  );

  return (
    <Drawer
      title={member ? `${member.email} 的用量（近 7 天）` : '成员用量'}
      width={720}
      open={member !== null}
      onClose={onClose}
      destroyOnHidden
      data-testid="member-usage-drawer"
    >
      {member ? (
        <Space direction="vertical" size={12} style={{ width: '100%' }}>
          <Alert
            type="info"
            showIcon
            message="服务端过滤，不是本地聚合"
            description={
              <Space direction="vertical" size={2}>
                <span>
                  请求：<code>GET /orgs/{orgId}/usage?user={member.user_id}&from=…&to=…&limit=20</code>
                </span>
                <span>
                  窗口是固定的近 7 天（UTC，服务端上限 31 天）；totals 覆盖整个过滤窗口。
                </span>
              </Space>
            }
          />
          <DataState query={usage} loadingRows={4} showStaleBanner>
            {(page) => (
              <Space direction="vertical" size={12} style={{ width: '100%' }}>
                <Space size={16} wrap>
                  <span>
                    事件 <strong>{formatNumber(page.totals.events)}</strong>
                  </span>
                  <span>
                    tokens <strong>{formatNumber(page.totals.total_tokens)}</strong>
                  </span>
                  <span>
                    演示消费 <strong>{formatCostCents(page.totals.cost_cents)}</strong>
                  </span>
                  <Tag color={page.scope === 'organization' ? 'blue' : 'default'}>
                    scope={page.scope}
                  </Tag>
                </Space>
                <Typography.Text type="secondary">
                  {formatTimestamp(page.window.from)} → {formatTimestamp(page.window.to)} ·{' '}
                  {page.totals.cost_note}
                </Typography.Text>
                <Table<UsageEvent>
                  size="small"
                  rowKey="id"
                  dataSource={page.items}
                  pagination={false}
                  locale={{ emptyText: '该成员在此窗口内没有计量事件' }}
                  scroll={{ x: 720 }}
                  columns={[
                    {
                      title: 'occurred_at',
                      dataIndex: 'occurred_at',
                      render: (value: string) => formatTimestamp(value),
                    },
                    { title: 'model', dataIndex: 'model' },
                    {
                      title: 'key_id',
                      dataIndex: 'key_id',
                      render: (value: string | null) => shortId(value),
                    },
                    {
                      title: 'tokens',
                      dataIndex: 'total_tokens',
                      render: (value: number) => formatNumber(value),
                    },
                    {
                      title: 'cost_cents',
                      dataIndex: 'cost_cents',
                      render: (value: string) => <Tag color="orange">{value}</Tag>,
                    },
                  ]}
                />
                <Typography.Text type="secondary">
                  {page.next_cursor
                    ? '窗口内还有更多事件：这里只显示第一页（20 条），完整分页请到「用量」页按 user 过滤。'
                    : '已显示该窗口内的全部事件。'}
                </Typography.Text>
              </Space>
            )}
          </DataState>
        </Space>
      ) : null}
    </Drawer>
  );
}

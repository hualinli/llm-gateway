/**
 * Platform-administrator surface.
 *
 * The frozen contract only has two platform-admin endpoints — create a
 * controlled user and activate/disable a user — so this page wires exactly
 * those. There is **no** user-list endpoint, so no table is rendered: a fake
 * list would be a lie.
 */

import { useState, type ReactNode } from 'react';
import {
  Alert,
  App as AntdApp,
  Button,
  Card,
  Form,
  Input,
  Result,
  Select,
  Space,
  Switch,
  Typography,
} from 'antd';

import { useCreateUser, useSetUserStatus } from '../api/queries';
import type { User, UserStatus } from '../api/types';
import { ErrorView, errorSummary } from '../components/states';
import { ContractGap, PageHeader } from '../components/PageHeader';
import { useSession } from '../session/SessionProvider';
import { formatTimestamp } from '../utils/format';

interface CreateUserForm {
  email: string;
  password: string;
  platform_admin: boolean;
  organizations?: string[];
}

export function PlatformAdminPage(): ReactNode {
  const { me, memberships } = useSession();
  const { message } = AntdApp.useApp();
  const createUser = useCreateUser();
  const setStatus = useSetUserStatus();
  const [createForm] = Form.useForm<CreateUserForm>();
  const [statusForm] = Form.useForm<{ user: string; status: UserStatus }>();
  const [lastCreated, setLastCreated] = useState<User | null>(null);
  const [lastUpdated, setLastUpdated] = useState<User | null>(null);

  if (me && !me.user.platform_admin) {
    return (
      <Result
        status="403"
        title="需要平台管理员权限"
        subTitle="users.platform_admin 是用户级标志，与组织角色无关：企业 admin/owner 不是平台管理员。服务端对这些端点固定返回 403 forbidden。"
      />
    );
  }

  const submitCreate = async (): Promise<void> => {
    const values = await createForm.validateFields();
    try {
      const user = await createUser.mutateAsync({
        email: values.email.trim(),
        password: values.password,
        platform_admin: values.platform_admin,
        ...(values.organizations && values.organizations.length > 0
          ? { organizations: values.organizations }
          : {}),
      });
      setLastCreated(user);
      createForm.resetFields();
      message.success(`已创建用户 ${user.email}`);
    } catch {
      // Rendered inline.
    }
  };

  const submitStatus = async (): Promise<void> => {
    const values = await statusForm.validateFields();
    try {
      const user = await setStatus.mutateAsync({
        userId: values.user.trim(),
        status: values.status,
      });
      setLastUpdated(user);
      message.success(`${user.email} 已设为 ${user.status}`);
      if (values.status === 'disabled') {
        message.warning('禁用会递增 auth_version：该用户所有已存在的会话立即失效。');
      }
    } catch (error) {
      message.error(`状态变更失败 — ${errorSummary(error)}`);
    }
  };

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <PageHeader
        title="平台管理"
        description="仅 users.platform_admin 可用；每个动作都会写入审计（actor / target / outcome / request_id）。"
      />

      <Alert
        type="info"
        showIcon
        message="契约只提供两个平台端点"
        description="POST /api/v1/admin/users（创建受控用户）与 POST /api/v1/admin/users/{user}/status（启用/禁用）。"
      />

      <Card size="small" title="创建受控用户">
        <Form<CreateUserForm>
          form={createForm}
          layout="vertical"
          initialValues={{ platform_admin: false }}
          style={{ maxWidth: 560 }}
        >
          <Form.Item
            name="email"
            label="邮箱"
            rules={[{ required: true, type: 'email', message: '请输入合法邮箱' }]}
          >
            <Input placeholder="member@example.com" autoComplete="off" />
          </Form.Item>
          <Form.Item
            name="password"
            label="初始密码"
            rules={[{ required: true, min: 8, message: '至少 8 位' }]}
            extra="只通过请求体发送一次；服务端存 bcrypt 哈希，审计与日志都不记录它。"
          >
            <Input.Password autoComplete="new-password" />
          </Form.Item>
          <Form.Item name="platform_admin" label="平台管理员" valuePropName="checked">
            <Switch />
          </Form.Item>
          <Form.Item
            name="organizations"
            label="加入组织（member 角色）"
            extra="只能选择你可见的组织；不可见的 id 会让服务端返回 404 且不创建用户。"
          >
            <Select
              mode="multiple"
              allowClear
              placeholder="可留空"
              options={memberships.map((membership) => ({
                value: membership.organization.id,
                label: `${membership.organization.name}（id=${membership.organization.id}）`,
              }))}
            />
          </Form.Item>
          <Button type="primary" loading={createUser.isPending} onClick={() => void submitCreate()}>
            创建用户
          </Button>
        </Form>
        {createUser.isError ? (
          <div style={{ marginTop: 12 }}>
            <ErrorView error={createUser.error} title="创建用户失败" />
          </div>
        ) : null}
        {lastCreated ? (
          <Alert
            style={{ marginTop: 12 }}
            type="success"
            showIcon
            message={`已创建 ${lastCreated.email}（user_id=${lastCreated.id}）`}
            description={`status=${lastCreated.status} · platform_admin=${String(lastCreated.platform_admin)} · created_at=${formatTimestamp(lastCreated.created_at)}`}
          />
        ) : null}
      </Card>

      <Card size="small" title="启用 / 禁用用户">
        <Form<{ user: string; status: UserStatus }>
          form={statusForm}
          layout="inline"
          initialValues={{ status: 'disabled' }}
          style={{ rowGap: 8 }}
        >
          <Form.Item
            name="user"
            label="user_id"
            rules={[{ required: true, pattern: /^[1-9][0-9]*$/, message: '十进制字符串 id' }]}
          >
            <Input placeholder="例如 3" style={{ width: 160 }} />
          </Form.Item>
          <Form.Item name="status" label="状态" rules={[{ required: true }]}>
            <Select
              style={{ width: 140 }}
              options={[
                { value: 'disabled', label: 'disabled' },
                { value: 'active', label: 'active' },
              ]}
            />
          </Form.Item>
          <Button type="primary" loading={setStatus.isPending} onClick={() => void submitStatus()}>
            提交
          </Button>
        </Form>
        {setStatus.isError ? (
          <div style={{ marginTop: 12 }}>
            <ErrorView error={setStatus.error} title="状态变更失败" />
          </div>
        ) : null}
        {lastUpdated ? (
          <Alert
            style={{ marginTop: 12 }}
            type="success"
            showIcon
            message={`${lastUpdated.email} 现在是 ${lastUpdated.status}`}
            description={`updated_at=${formatTimestamp(lastUpdated.updated_at)}。重新启用不会恢复旧会话。`}
          />
        ) : null}
      </Card>

      <Typography.Text type="secondary">
        当前身份：{me?.user.email ?? '—'}（user_id={me?.user.id ?? '—'}）
      </Typography.Text>

      <ContractGap title="没有用户列表 / 组织列表 / 充值端点">
        GET /api/v1/admin/users 不存在：平台管理员无法在界面上枚举用户，只能凭已知
        user_id 操作。“管理员额度调整”需要 T05/T06 的账本端点，本页不提供任何充值与价格控件。
      </ContractGap>
    </Space>
  );
}

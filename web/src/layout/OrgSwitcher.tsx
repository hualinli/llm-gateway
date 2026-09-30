/**
 * Organization switcher: one entry per membership returned by
 * `GET /api/v1/organizations` (never a hard-coded list), plus the enterprise
 * organization creation dialog (`POST /api/v1/organizations`).
 *
 * Switching organizations navigates to the new `/orgs/:orgId/overview` route,
 * which both remounts the org-scoped providers and drops the previous
 * organization's cached queries.
 */

import { useState, type ReactNode } from 'react';
import { App as AntdApp, Button, Form, Input, Modal, Select, Space, Tag } from 'antd';
import { PlusOutlined } from '@ant-design/icons';

import { useCreateOrganization } from '../api/queries';
import type { Membership } from '../api/types';
import { useActiveOrg, useSession } from '../session/SessionProvider';
import { ErrorView } from '../components/states';
import { ORG_TYPE_LABELS, ROLE_LABELS, ROLE_TAG_COLOR } from '../utils/permissions';

export function OrgSwitcher(): ReactNode {
  const { memberships, switchOrg } = useSession();
  const { orgId } = useActiveOrg();
  const { message } = AntdApp.useApp();
  const [createOpen, setCreateOpen] = useState(false);
  const [form] = Form.useForm<{ name: string }>();
  const createOrg = useCreateOrganization();

  const options = memberships.map((membership: Membership) => ({
    value: membership.organization.id,
    label: (
      <Space size={6}>
        <span>{membership.organization.name}</span>
        <Tag color={ROLE_TAG_COLOR[membership.role]} style={{ marginInlineEnd: 0 }}>
          {ROLE_LABELS[membership.role]}
        </Tag>
        <Tag style={{ marginInlineEnd: 0 }}>
          {ORG_TYPE_LABELS[membership.organization.type] ?? membership.organization.type}
        </Tag>
      </Space>
    ),
    title: membership.organization.name,
  }));

  const submitCreate = async (): Promise<void> => {
    const values = await form.validateFields();
    try {
      const created = await createOrg.mutateAsync(values.name.trim());
      message.success(`已创建组织 ${created.organization.name}`);
      setCreateOpen(false);
      form.resetFields();
      switchOrg(created.organization.id);
    } catch {
      // The dialog renders the error below; nothing to add.
    }
  };

  return (
    <>
      <Space size={8} wrap>
        <Select
          aria-label="切换组织"
          style={{ minWidth: 220 }}
          placeholder="选择组织"
          value={orgId ?? null}
          options={options}
          onChange={(value: string) => switchOrg(value)}
          notFoundContent="没有可见组织"
        />
        <Button
          icon={<PlusOutlined />}
          onClick={() => setCreateOpen(true)}
          aria-label="创建企业组织"
        >
          新建组织
        </Button>
      </Space>

      <Modal
        title="创建企业组织"
        open={createOpen}
        onCancel={() => setCreateOpen(false)}
        onOk={() => {
          void submitCreate();
        }}
        okText="创建"
        cancelText="取消"
        confirmLoading={createOrg.isPending}
        okButtonProps={{ disabled: createOrg.isPending }}
        destroyOnHidden
      >
        <Form form={form} layout="vertical" onFinish={() => void submitCreate()}>
          <Form.Item
            name="name"
            label="组织名称"
            rules={[{ required: true, message: '请输入组织名称' }]}
          >
            <Input maxLength={120} placeholder="例如 acme" autoFocus />
          </Form.Item>
        </Form>
        <p style={{ color: 'rgba(0,0,0,0.45)', marginBottom: 0 }}>
          创建者会成为该组织的 <strong>owner</strong>（服务端在同一事务内写入成员关系）。
        </p>
        {createOrg.isError ? <ErrorView error={createOrg.error} /> : null}
      </Modal>
    </>
  );
}

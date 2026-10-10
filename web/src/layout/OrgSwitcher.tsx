import type { ReactNode } from 'react';
import { Select, Space, Tag } from 'antd';

import type { Membership } from '../api/types';
import { useActiveOrg, useSession } from '../session/SessionProvider';
import { ORG_TYPE_LABELS, ROLE_LABELS } from '../utils/permissions';

export function OrgSwitcher(): ReactNode {
  const { memberships, switchOrg } = useSession();
  const { orgId } = useActiveOrg();
  const options = memberships.map((membership: Membership) => ({
    value: membership.organization.id,
    label: membership.organization.type === 'personal' ? '我的个人账户' : membership.organization.name,
    title: membership.organization.name,
  }));

  return (
    <Space size={8} wrap>
      <div className="account-selector"><span className="account-selector-label">当前使用账户</span>
      <Select
        aria-label="切换账户"
        style={{ minWidth: 230, maxWidth: 'min(72vw, 360px)' }}
        placeholder="选择账户"
        value={orgId ?? null}
        options={options}
        onChange={(value: string) => switchOrg(value)}
        optionRender={(option) => {
          const membership = memberships.find((item) => item.organization.id === option.value);
          return (
            <Space size={6}>
              <span>{membership?.organization.name}</span>
              {membership ? <Tag color={membership.organization.type === 'personal' ? 'default' : 'blue'}>{ORG_TYPE_LABELS[membership.organization.type]}</Tag> : null}
              {membership ? <Tag>{ROLE_LABELS[membership.role]}</Tag> : null}
            </Space>
          );
        }}
        notFoundContent="暂无可用账户"
      />
      </div>
    </Space>
  );
}

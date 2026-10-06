import { Component, useMemo, useState, type ErrorInfo, type ReactNode } from 'react';
import { Link, Navigate, Outlet, useLocation, useNavigate } from 'react-router-dom';
import {
  Button,
  Drawer,
  Dropdown,
  Grid,
  Layout,
  Menu,
  Result,
  Space,
  Spin,
  Tag,
  type MenuProps,
} from 'antd';
import {
  AppstoreOutlined,
  BarChartOutlined,
  DashboardOutlined,
  KeyOutlined,
  LogoutOutlined,
  MenuOutlined,
  SettingOutlined,
  ShopOutlined,
  TagsOutlined,
  TeamOutlined,
  UserOutlined,
  WalletOutlined,
} from '@ant-design/icons';

import { useActiveOrg, useSession } from '../session/SessionProvider';
import { PlaygroundKeyProvider } from '../session/PlaygroundKeyProvider';
import { CAN, ROLE_LABELS } from '../utils/permissions';
import { OrgSwitcher } from './OrgSwitcher';

const { Header, Content, Sider } = Layout;

export function RequireSession({ children }: { children: ReactNode }): ReactNode {
  const { isLoading, isAuthenticated, error } = useSession();
  const location = useLocation();
  if (isLoading) return <div className="app-centered"><Spin size="large" /></div>;
  if (error) return <div className="app-centered"><Result status="error" title="暂时无法打开工作台" subTitle="请检查网络后重试。" /></div>;
  if (!isAuthenticated) return <Navigate to="/login" replace state={{ from: location.pathname }} />;
  return <>{children}</>;
}

class ContentErrorBoundary extends Component<{ children: ReactNode }, { error: Error | null }> {
  override state: { error: Error | null } = { error: null };
  static getDerivedStateFromError(error: Error): { error: Error } { return { error }; }
  override componentDidCatch(error: Error, _info: ErrorInfo): void {
    console.error('[web] render error', error.message);
  }
  override render(): ReactNode {
    if (this.state.error) return <Result status="error" title="页面暂时无法显示" extra={<Button type="primary" onClick={() => this.setState({ error: null })}>重试</Button>} />;
    return this.props.children;
  }
}

export function buildMenuItems(
  orgId: string | null,
  membership: ReturnType<typeof useActiveOrg>['membership'],
  platformAdmin: boolean,
): MenuProps['items'] {
  const items: NonNullable<MenuProps['items']> = [];
  const base = orgId ? `/orgs/${encodeURIComponent(orgId)}` : null;
  if (base && membership) {
    const personal = membership.organization.type === 'personal';
    const account: NonNullable<MenuProps['items']> = [
      { key: `${base}/overview`, icon: <DashboardOutlined />, label: '账户概览' },
      { key: `${base}/models`, icon: <AppstoreOutlined />, label: '可用模型' },
      { key: `${base}/keys`, icon: <KeyOutlined />, label: '我的 API Key' },
      { key: `${base}/usage`, icon: <BarChartOutlined />, label: personal ? '我的消费' : '用量与额度' },
    ];
    if (CAN.manageBilling(membership)) {
      account.push({ key: `${base}/billing`, icon: <WalletOutlined />, label: personal ? '账户流水' : '企业账单' });
    }
    if (!personal && CAN.manageMembers(membership.role)) {
      account.push({ key: `${base}/members`, icon: <TeamOutlined />, label: '成员与申请' });
      account.push({ key: `${base}/audit`, icon: <TagsOutlined />, label: '操作记录' });
    }
    items.push({ type: 'group', label: personal ? '个人账户' : '企业账户', children: account });
  }

  items.push({ type: 'group', label: '账号', children: [
    { key: '/settings', icon: <UserOutlined />, label: '个人设置' },
  ] });

  if (platformAdmin) {
    items.push({ type: 'group', label: '平台管理', children: [
      { key: '/platform/organizations', icon: <ShopOutlined />, label: '企业与开户' },
      { key: '/platform/providers', icon: <SettingOutlined />, label: '上游接入' },
      { key: '/platform/models', icon: <AppstoreOutlined />, label: '模型可用范围' },
      { key: '/platform/prices', icon: <TagsOutlined />, label: '模型价格' },
      { key: '/platform/billing', icon: <WalletOutlined />, label: '平台账务' },
    ] });
  }
  return items;
}

function activeMenuKey(pathname: string, items: MenuProps['items']): string {
  const keys: string[] = [];
  for (const item of items ?? []) {
    if (item && 'children' in item && Array.isArray(item.children)) {
      for (const child of item.children) if (child && 'key' in child && typeof child.key === 'string') keys.push(child.key);
    }
  }
  return keys.filter((key) => pathname === key || pathname.startsWith(`${key}/`)).sort((a, b) => b.length - a.length)[0] ?? pathname;
}

export function AppShell(): ReactNode {
  const { me, logout } = useSession();
  const { orgId, membership, role } = useActiveOrg();
  const screens = Grid.useBreakpoint();
  const isNarrow = screens.lg !== true;
  const [drawerOpen, setDrawerOpen] = useState(false);
  const navigate = useNavigate();
  const { pathname } = useLocation();
  const platformAdmin = me?.user.platform_admin === true;
  const menuItems = useMemo(() => buildMenuItems(orgId, membership, platformAdmin), [membership, orgId, platformAdmin]);
  const selectedKey = activeMenuKey(pathname, menuItems);

  const menu = <Menu mode="inline" items={menuItems ?? []} selectedKeys={[selectedKey]} onClick={({ key }) => { setDrawerOpen(false); navigate(key); }} style={{ borderInlineEnd: 'none' }} />;
  const onLogout = async (): Promise<void> => {
    try { await logout(); } catch { /* Local session is cleared even if revocation cannot be reached. */ }
  };
  const roleLabel = membership?.organization.type === 'personal' ? '个人账户管理员' : role ? ROLE_LABELS[role] : '';

  return (
    <Layout style={{ minHeight: '100vh' }}>
      {isNarrow ? (
        <Drawer title="导航" placement="left" open={drawerOpen} onClose={() => setDrawerOpen(false)} width={280} styles={{ body: { padding: 0 } }}>{menu}</Drawer>
      ) : (
        <Sider theme="light" width={248} style={{ borderInlineEnd: '1px solid #f0f0f0' }}>
          <div className="app-brand"><Link to="/">LLM Gateway</Link><span>个人与团队模型账户</span></div>
          {menu}
        </Sider>
      )}
      <Layout>
        <Header className="app-header">
          <Space size={8} wrap>
            {isNarrow ? <Button icon={<MenuOutlined />} onClick={() => setDrawerOpen(true)} aria-label="打开导航" /> : null}
            <OrgSwitcher />
            {membership ? <Tag color={membership.organization.type === 'personal' ? 'default' : 'blue'}>{roleLabel}</Tag> : null}
            {membership?.organization.status === 'suspended' ? <Tag color="red">账户已暂停</Tag> : null}
            {membership?.organization.type === 'enterprise' && membership.status === 'suspended' ? <Tag color="orange">成员资格已暂停</Tag> : null}
          </Space>
          <Dropdown menu={{ items: [
            { key: 'email', label: me?.user.email ?? '—', disabled: true },
            { type: 'divider' },
            { key: 'settings', icon: <UserOutlined />, label: '个人设置', onClick: () => navigate('/settings') },
            { key: 'logout', icon: <LogoutOutlined />, label: '退出登录', onClick: () => void onLogout() },
          ] }} trigger={['click']} placement="bottomRight">
            <Button icon={<UserOutlined />} aria-label="账户菜单"><span className="app-header-email">{me?.user.email ?? '账户'}</span></Button>
          </Dropdown>
        </Header>
        <Content className="app-content"><ContentErrorBoundary><Outlet /></ContentErrorBoundary></Content>
      </Layout>
    </Layout>
  );
}

export function OrgLayout(): ReactNode {
  const { orgId, membership } = useActiveOrg();
  const { isLoading } = useSession();
  if (!orgId) return <Navigate to="/" replace />;
  if (isLoading) return <div className="app-centered"><Spin /></div>;
  if (!membership) {
    return <Result status="warning" title="无法打开这个账户" subTitle="请从账户切换菜单中选择你有权访问的账户。" extra={<Link to="/"><Button type="primary">返回账户</Button></Link>} />;
  }
  return <PlaygroundKeyProvider key={orgId} orgId={orgId}><Outlet /></PlaygroundKeyProvider>;
}

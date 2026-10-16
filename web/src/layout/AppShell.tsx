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
      { key: `${base}/overview`, icon: <DashboardOutlined />, label: '工作台' },
    ];
    if (membership.status !== 'suspended') {
      account.push(
        { key: `${base}/models`, icon: <AppstoreOutlined />, label: '可用模型' },
        { key: `${base}/keys`, icon: <KeyOutlined />, label: '我的 API Key' },
      );
    }
    account.push({ key: `${base}/usage`, icon: <BarChartOutlined />, label: personal ? '我的消费' : '用量与额度' });
    if (personal && CAN.manageBilling(membership)) {
      account.push({ key: `${base}/billing`, icon: <WalletOutlined />, label: personal ? '账户流水' : '企业账单' });
    }
    items.push({ type: 'group', label: personal ? '我的模型账户' : '模型使用', children: account });
    if (!personal && membership.status === 'active' && CAN.manageMembers(membership.role)) {
      items.push({ type: 'group', label: '企业管理', children: [
        { key: `${base}/billing`, icon: <WalletOutlined />, label: '企业账单' },
        { key: `${base}/members`, icon: <TeamOutlined />, label: '成员与申请' },
        { key: `${base}/audit`, icon: <TagsOutlined />, label: '操作记录' },
      ] });
    }
  }
  if (!base && platformAdmin) {
    items.push({ type: 'group', label: '平台运营', children: [
      { key: '/platform/overview', icon: <DashboardOutlined />, label: '平台概览' },
      { key: '/platform/organizations', icon: <ShopOutlined />, label: '企业与开户' },
      { key: '/platform/billing', icon: <WalletOutlined />, label: '平台账务' },
    ] });
    items.push({ type: 'group', label: '模型服务', children: [
      { key: '/platform/providers', icon: <SettingOutlined />, label: '上游接入' },
      { key: '/platform/models', icon: <AppstoreOutlined />, label: '模型可用范围' },
      { key: '/platform/prices', icon: <TagsOutlined />, label: '模型价格' },
    ] });
  }
  items.push({ type: 'group', label: '我的账号', children: [
    { key: '/settings', icon: <UserOutlined />, label: '个人设置' },
  ] });
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
  const { me, memberships, logout } = useSession();
  const { orgId, membership, role } = useActiveOrg();
  const screens = Grid.useBreakpoint();
  const isNarrow = screens.lg !== true;
  const [drawerOpen, setDrawerOpen] = useState(false);
  const navigate = useNavigate();
  const { pathname } = useLocation();
  const platformAdmin = me?.user.platform_admin === true;
  const inPlatform = pathname.startsWith('/platform');
  const personalAccount = memberships.find(item => item.organization.type === 'personal') ?? memberships[0];
  const menuItems = useMemo(() => buildMenuItems(orgId, membership, platformAdmin), [membership, orgId, platformAdmin]);
  const selectedKey = activeMenuKey(pathname, menuItems);

  const menu = <Menu mode="inline" items={menuItems ?? []} selectedKeys={[selectedKey]} onClick={({ key }) => { setDrawerOpen(false); navigate(key); }} style={{ borderInlineEnd: 'none' }} />;
  const onLogout = async (): Promise<void> => {
    try { await logout(); } catch { /* Local session is cleared even if revocation cannot be reached. */ }
  };
  const roleLabel = membership?.organization.type === 'personal' ? '个人使用' : role ? ROLE_LABELS[role] : '';

  return (
    <Layout className="app-layout" style={{ minHeight: '100vh' }}>
      {isNarrow ? (
        <Drawer title="导航" placement="left" open={drawerOpen} onClose={() => setDrawerOpen(false)} width={280} styles={{ body: { padding: 0 } }}>{menu}</Drawer>
      ) : (
        <Sider theme="light" width={240} className="app-sidebar">
          <div className="app-brand"><Link to="/"><span className="brand-mark">G</span>LLM Gateway</Link><span>模型服务与用量管理</span></div>
          <div className="sidebar-scope"><span>{inPlatform ? '平台管理员' : membership?.organization.type === 'enterprise' ? '企业工作区' : '个人工作区'}</span><strong>{inPlatform ? '平台管理' : membership?.organization.name ?? '我的账号'}</strong></div>
          {menu}
          <div className="sidebar-caption">{inPlatform ? '上游接入 · 客户账户 · 平台账务' : '你的 Key，你的用量，清晰的账单。'}</div>
        </Sider>
      )}
      <Layout>
        <Header className="app-header">
          <Space size={8} wrap>
            {isNarrow ? <Button icon={<MenuOutlined />} onClick={() => setDrawerOpen(true)} aria-label="打开导航" /> : null}
            {inPlatform ? <div className="workspace-context"><strong>平台管理</strong><span>全部个人与企业账户</span></div> : <OrgSwitcher />}
            {membership && roleLabel ? <Tag data-testid="active-role-tag" data-org-type={membership.organization.type} color={membership.organization.type === 'personal' ? 'default' : 'blue'}>{roleLabel}</Tag> : null}
            {membership?.organization.status === 'suspended' ? <Tag color="red">账户已暂停</Tag> : null}
            {membership?.organization.type === 'enterprise' && membership.status === 'suspended' ? <Tag color="orange">成员资格已暂停</Tag> : null}
          </Space>
          <Space size={10}>
          {platformAdmin ? <Button className="workspace-mode-button" disabled={inPlatform && !personalAccount} onClick={() => navigate(inPlatform && personalAccount ? `/orgs/${encodeURIComponent(personalAccount.organization.id)}/overview` : '/platform/overview')}>{inPlatform ? '我的使用账户' : '进入平台管理'}</Button> : null}
          <Dropdown menu={{ items: [
            { key: 'email', label: me?.user.email ?? '—', disabled: true },
            { type: 'divider' },
            { key: 'join', label: '申请加入企业', onClick: () => navigate('/join') },
            { key: 'activate', label: '兑换企业开户码', onClick: () => navigate('/activate') },
            { key: 'settings', icon: <UserOutlined />, label: '个人设置', onClick: () => navigate('/settings') },
            { key: 'logout', icon: <LogoutOutlined />, label: '退出登录', onClick: () => void onLogout() },
          ] }} trigger={['click']} placement="bottomRight">
            <Button icon={<UserOutlined />} aria-label="账户菜单"><span className="app-header-email">{me?.user.email ?? '账户'}</span></Button>
          </Dropdown>
          </Space>
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

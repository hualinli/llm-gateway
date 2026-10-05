/**
 * Application shell: responsive navigation (Sider at ≥ lg, Drawer below it, so
 * both the 1440 and the 390 layout are usable), the organization switcher, the
 * identity menu and the org-scoped providers.
 */

import { Component, useMemo, useState, type ErrorInfo, type ReactNode } from 'react';
import { Link, Navigate, Outlet, useLocation, useNavigate } from 'react-router-dom';
import {
  App as AntdApp,
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
  Typography,
  type MenuProps,
} from 'antd';
import {
  AppstoreOutlined,
  AuditOutlined,
  BarChartOutlined,
  DashboardOutlined,
  KeyOutlined,
  LogoutOutlined,
  MenuOutlined,
  SettingOutlined,
  TagsOutlined,
  TeamOutlined,
  UserOutlined,
  WalletOutlined,
} from '@ant-design/icons';

import { ErrorView } from '../components/states';
import { useActiveOrg, useSession } from '../session/SessionProvider';
import { PlaygroundKeyProvider } from '../session/PlaygroundKeyProvider';
import { CAN, ORG_TYPE_LABELS, ROLE_LABELS, ROLE_TAG_COLOR } from '../utils/permissions';
import { OrgSwitcher } from './OrgSwitcher';

const { Header, Content, Sider } = Layout;

/* --------------------------------------------------------------- guards */

/** Blocks the app shell until `GET /api/v1/me` succeeded; 401 -> login route. */
export function RequireSession({ children }: { children: ReactNode }): ReactNode {
  const { isLoading, isAuthenticated, error } = useSession();
  const location = useLocation();

  if (isLoading) {
    return (
      <div className="app-centered">
        <Space direction="vertical" align="center">
          <Spin size="large" />
          <Typography.Text type="secondary">正在读取会话（GET /api/v1/me）…</Typography.Text>
        </Space>
      </div>
    );
  }

  if (error) {
    return (
      <div className="app-centered">
        <div style={{ maxWidth: 640, width: '100%' }}>
          <ErrorView error={error} title="无法读取会话" />
        </div>
      </div>
    );
  }

  if (!isAuthenticated) {
    return <Navigate to="/login" replace state={{ from: location.pathname }} />;
  }

  return <>{children}</>;
}

/** Keeps a rendering bug from producing a blank page. */
class ContentErrorBoundary extends Component<
  { children: ReactNode },
  { error: Error | null }
> {
  override state: { error: Error | null } = { error: null };

  static getDerivedStateFromError(error: Error): { error: Error } {
    return { error };
  }

  override componentDidCatch(error: Error, info: ErrorInfo): void {
    // Deliberately logs only the message and component stack: request bodies and
    // credentials must never reach the console.
    console.error('[web] render error', error.message, info.componentStack);
  }

  override render(): ReactNode {
    if (this.state.error) {
      return (
        <Result
          status="error"
          title="页面渲染失败"
          subTitle={this.state.error.message}
          extra={
            <Button type="primary" onClick={() => this.setState({ error: null })}>
              重试渲染
            </Button>
          }
        />
      );
    }
    return this.props.children;
  }
}

/* ------------------------------------------------------------- navigation */

export function buildMenuItems(
  orgId: string | null,
  role: ReturnType<typeof useActiveOrg>['role'],
  platformAdmin: boolean,
): MenuProps['items'] {
  const items: NonNullable<MenuProps['items']> = [];
  const base = orgId ? `/orgs/${encodeURIComponent(orgId)}` : null;

  if (base) {
    items.push({
      type: 'group',
      label: '个人',
      children: [
        { key: `${base}/overview`, icon: <DashboardOutlined />, label: '概览' },
        { key: `${base}/models`, icon: <AppstoreOutlined />, label: '模型目录与 Playground' },
        ...(CAN.manageOwnKeys(role)
          ? [{ key: `${base}/keys`, icon: <KeyOutlined />, label: '我的 API Keys' }]
          : []),
        { key: `${base}/usage`, icon: <BarChartOutlined />, label: '用量' },
      ],
    });

    const enterprise: NonNullable<MenuProps['items']> = [];
    // The billing page is reachable by every role: the server returns the real
    // `403 forbidden` for a plain member and the page renders it as such.
    enterprise.push({ key: `${base}/billing`, icon: <WalletOutlined />, label: '资金与结算' });
    if (CAN.viewMembers(role)) {
      enterprise.push({ key: `${base}/members`, icon: <TeamOutlined />, label: '成员与邀请' });
    }
    if (CAN.viewAudit(role)) {
      enterprise.push({ key: `${base}/audit`, icon: <AuditOutlined />, label: '审计' });
    }
    if (enterprise.length > 0) {
      items.push({ type: 'group', label: '企业管理', children: enterprise });
    }
  }

  if (platformAdmin) {
    items.push({
      type: 'group',
      label: '平台管理',
      children: [
        { key: '/platform', icon: <SettingOutlined />, label: '用户与状态' },
        { key: '/platform/prices', icon: <TagsOutlined />, label: '价格版本' },
        { key: '/platform/billing', icon: <WalletOutlined />, label: '额度调整与异常' },
      ],
    });
  }

  return items;
}

function selectedKey(pathname: string, items: MenuProps['items']): string {
  const keys: string[] = [];
  for (const item of items ?? []) {
    if (item && 'children' in item && Array.isArray(item.children)) {
      for (const child of item.children) {
        if (child && 'key' in child && typeof child.key === 'string') keys.push(child.key);
      }
    }
  }
  const match = keys
    .filter((key) => pathname === key || pathname.startsWith(`${key}/`))
    .sort((a, b) => b.length - a.length)[0];
  return match ?? pathname;
}

export function AppShell(): ReactNode {
  const { me, logout, memberships } = useSession();
  const { orgId, membership, role } = useActiveOrg();
  const screens = Grid.useBreakpoint();
  const isNarrow = screens.lg !== true;
  const [drawerOpen, setDrawerOpen] = useState(false);
  const navigate = useNavigate();
  const { pathname } = useLocation();
  const { message } = AntdApp.useApp();

  const menuItems = useMemo(
    () => buildMenuItems(orgId, role, me?.user.platform_admin === true),
    [me?.user.platform_admin, orgId, role],
  );
  const activeKey = selectedKey(pathname, menuItems);

  const menu = (
    <Menu
      mode="inline"
      items={menuItems ?? []}
      selectedKeys={[activeKey]}
      onClick={({ key }) => {
        setDrawerOpen(false);
        navigate(key);
      }}
      style={{ borderInlineEnd: 'none' }}
    />
  );

  const onLogout = async (): Promise<void> => {
    try {
      await logout();
      message.success('已退出登录（会话已在服务端撤销）');
    } catch {
      // `logout` clears local state even when the revoke call fails; the user is
      // already on the login route.
    }
  };

  const identityMenu: MenuProps = {
    items: [
      {
        key: 'identity',
        label: (
          <Space direction="vertical" size={0}>
            <strong>{me?.user.email ?? '—'}</strong>
            <Typography.Text type="secondary">
              user_id={me?.user.id ?? '—'} · {me?.user.platform_admin ? '平台管理员' : '普通用户'}
            </Typography.Text>
            <Typography.Text type="secondary">
              可见组织 {memberships.length} 个
            </Typography.Text>
          </Space>
        ),
        disabled: true,
      },
      { type: 'divider' },
      {
        key: 'logout',
        icon: <LogoutOutlined />,
        label: '退出登录',
        onClick: () => {
          void onLogout();
        },
      },
    ],
  };

  return (
    <Layout style={{ minHeight: '100vh' }}>
      {isNarrow ? (
        <Drawer
          title="导航"
          placement="left"
          open={drawerOpen}
          onClose={() => setDrawerOpen(false)}
          width={280}
          styles={{ body: { padding: 0 } }}
        >
          {menu}
        </Drawer>
      ) : (
        <Sider theme="light" width={248} breakpoint="lg" style={{ borderInlineEnd: '1px solid #f0f0f0' }}>
          <div className="app-brand">
            <Link to="/">LLM Gateway</Link>
            <span>模型 API 与团队计费平台</span>
          </div>
          {menu}
        </Sider>
      )}

      <Layout>
        <Header className="app-header">
          <Space size={8} wrap>
            {isNarrow ? (
              <Button
                icon={<MenuOutlined />}
                onClick={() => setDrawerOpen(true)}
                aria-label="打开导航"
              />
            ) : null}
            <OrgSwitcher />
            {membership ? (
              <>
                <Tag color={ROLE_TAG_COLOR[membership.role]}>{ROLE_LABELS[membership.role]}</Tag>
                <Tag>{ORG_TYPE_LABELS[membership.organization.type] ?? membership.organization.type}</Tag>
              </>
            ) : null}
          </Space>
          <Dropdown menu={identityMenu} trigger={['click']} placement="bottomRight">
            <Button icon={<UserOutlined />} aria-label="账户菜单">
              <span className="app-header-email">{me?.user.email ?? '未登录'}</span>
            </Button>
          </Dropdown>
        </Header>

        <Content className="app-content">
          <ContentErrorBoundary>
            <Outlet />
          </ContentErrorBoundary>
        </Content>
      </Layout>
    </Layout>
  );
}

/* -------------------------------------------------------- org-scoped shell */

/**
 * Wraps the organization routes: validates that `:orgId` is one of the
 * caller's memberships (the server never lists the others anyway) and mounts
 * the Playground key provider with `key={orgId}` so a switch cannot leak the
 * previous organization's in-memory API key.
 */
export function OrgLayout(): ReactNode {
  const { orgId, membership } = useActiveOrg();
  const { isLoading } = useSession();

  if (!orgId) return <Navigate to="/" replace />;
  if (isLoading) return <Spin />;
  if (!membership) {
    return (
      <Result
        status="404"
        title="组织不可见（404 not_found）"
        subTitle="服务端对非成员固定返回 404，不泄露组织是否存在；请从组织切换器中选择一个可见组织。"
        extra={
          <Link to="/">
            <Button type="primary">返回</Button>
          </Link>
        }
      />
    );
  }

  return (
    <PlaygroundKeyProvider key={orgId} orgId={orgId}>
      <Outlet />
    </PlaygroundKeyProvider>
  );
}

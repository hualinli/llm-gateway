/**
 * Route table. Organization state lives in the URL (`/orgs/:orgId/...`), so a
 * reload keeps the active organization and a switch is an ordinary navigation.
 */

import type { ReactNode } from 'react';
import { Navigate, Route, Routes } from 'react-router-dom';

import { AppShell, OrgLayout, RequireSession } from './layout/AppShell';
import { AcceptInvitationPage } from './pages/AcceptInvitationPage';
import { AuditPage } from './pages/AuditPage';
import { BillingPage } from './pages/BillingPage';
import { HomeRedirect } from './pages/HomeRedirect';
import { KeysPage } from './pages/KeysPage';
import { LoginPage } from './pages/LoginPage';
import { MembersPage } from './pages/MembersPage';
import { ModelCatalogPage } from './pages/ModelCatalogPage';
import { NotFoundPage } from './pages/NotFoundPage';
import { OverviewPage } from './pages/OverviewPage';
import { PlatformAdminPage } from './pages/PlatformAdminPage';
import { PlatformBillingPage } from './pages/PlatformBillingPage';
import { PricesPage } from './pages/PricesPage';
import { UsagePage } from './pages/UsagePage';

export function App(): ReactNode {
  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      <Route
        path="/invitations/:token"
        element={
          <RequireSession>
            <AcceptInvitationPage />
          </RequireSession>
        }
      />

      <Route
        element={
          <RequireSession>
            <AppShell />
          </RequireSession>
        }
      >
        <Route path="/" element={<HomeRedirect />} />
        <Route path="/platform" element={<PlatformAdminPage />} />
        <Route path="/platform/prices" element={<PricesPage />} />
        <Route path="/platform/billing" element={<PlatformBillingPage />} />

        <Route path="/orgs/:orgId" element={<OrgLayout />}>
          <Route index element={<Navigate to="overview" replace />} />
          <Route path="overview" element={<OverviewPage />} />
          <Route path="models" element={<ModelCatalogPage />} />
          <Route path="keys" element={<KeysPage />} />
          <Route path="usage" element={<UsagePage />} />
          <Route path="billing" element={<BillingPage />} />
          <Route path="members" element={<MembersPage />} />
          <Route path="audit" element={<AuditPage />} />
          <Route path="*" element={<NotFoundPage />} />
        </Route>

        <Route path="*" element={<NotFoundPage />} />
      </Route>
    </Routes>
  );
}

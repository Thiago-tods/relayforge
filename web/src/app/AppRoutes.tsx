import { Navigate, Route, Routes } from 'react-router-dom'
import { PublicOnly, RequireAuth } from '../auth/guards'
import { LoginPage } from '../features/auth/LoginPage'
import { RegisterPage } from '../features/auth/RegisterPage'
import { ActivityPage } from '../features/operations/ActivityPage'
import { DeliveryDetailPage } from '../features/deliveries/DeliveryDetailPage'
import { QueuePage } from '../features/operations/QueuePage'
import { OrgPickerPage } from '../features/orgs/OrgPickerPage'
import { OverviewPage } from '../features/orgs/OverviewPage'
import { OrgLayout } from '../org/OrgLayout'
import { NotFoundPage } from './NotFoundPage'
import { Shell } from './Shell'

export function AppRoutes() {
  return (
    <Routes>
      <Route element={<PublicOnly />}>
        <Route path="/login" element={<LoginPage />} />
        <Route path="/register" element={<RegisterPage />} />
      </Route>
      <Route element={<RequireAuth />}>
        <Route element={<Shell />}>
          <Route path="/" element={<Navigate to="/orgs" replace />} />
          <Route path="/orgs" element={<OrgPickerPage />} />
          <Route path="/o/:orgId" element={<OrgLayout />}>
            <Route index element={<OverviewPage />} />
            <Route path="operations/queue" element={<QueuePage />} />
            <Route path="operations/activity" element={<ActivityPage />} />
            <Route path="operations/deliveries/:deliveryId" element={<DeliveryDetailPage />} />
            <Route path="*" element={<NotFoundPage />} />
          </Route>
          <Route path="*" element={<NotFoundPage />} />
        </Route>
      </Route>
    </Routes>
  )
}

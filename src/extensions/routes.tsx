import { lazy } from 'react';
import type { RouteObject } from 'react-router-dom';

const UsersPage = lazy(() =>
  import('./pages/UsersPage').then((m) => ({ default: m.UsersPage }))
);
const UserDetailPage = lazy(() =>
  import('./pages/UserDetailPage').then((m) => ({ default: m.UserDetailPage }))
);

export const extensionRoutes: RouteObject[] = [
  { path: '/custom/users', element: <UsersPage /> },
  { path: '/custom/users/:index', element: <UserDetailPage /> }
];

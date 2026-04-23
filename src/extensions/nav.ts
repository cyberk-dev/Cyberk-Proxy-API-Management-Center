import type { ReactNode } from 'react';
import { createElement } from 'react';
import type { TFunction } from 'i18next';

export interface ExtensionNavItem {
  path: string;
  label: string;
  icon?: ReactNode;
}

function UsersIcon(): ReactNode {
  return createElement(
    'svg',
    {
      width: 18,
      height: 18,
      viewBox: '0 0 24 24',
      fill: 'none',
      stroke: 'currentColor',
      strokeWidth: 2,
      strokeLinecap: 'round',
      strokeLinejoin: 'round',
      'aria-hidden': 'true',
      focusable: 'false'
    },
    createElement('path', { key: 'p1', d: 'M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2' }),
    createElement('circle', { key: 'p2', cx: 9, cy: 7, r: 4 }),
    createElement('path', { key: 'p3', d: 'M22 21v-2a4 4 0 0 0-3-3.87' }),
    createElement('path', { key: 'p4', d: 'M16 3.13a4 4 0 0 1 0 7.75' })
  );
}

export function extensionNavItems(t: TFunction): ExtensionNavItem[] {
  return [{ path: '/custom/users', label: t('extensions:nav.users'), icon: UsersIcon() }];
}

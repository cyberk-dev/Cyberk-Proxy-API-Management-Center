/**
 * Isolated extensions module — all custom code lives under this folder so that
 * upstream pulls don't conflict with local work.
 *
 * Entry points consumed by upstream files (via spread inside marker comments):
 *   - extensionRoutes    → src/router/MainRoutes.tsx
 *   - extensionNavItems  → src/components/layout/MainLayout.tsx
 */

import i18n from '@/i18n';
import enExt from './i18n/en';
import zhCNExt from './i18n/zh-CN';

// Register i18n bundles on the already-initialised instance. Runs at first import.
i18n.addResourceBundle('en', 'extensions', enExt, true, true);
i18n.addResourceBundle('zh-CN', 'extensions', zhCNExt, true, true);
i18n.addResourceBundle('zh-TW', 'extensions', zhCNExt, true, true);
i18n.addResourceBundle('ru', 'extensions', enExt, true, true);

export { extensionRoutes } from './routes';
export { extensionNavItems } from './nav';

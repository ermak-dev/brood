// FEATURE: Session restore on reload (#256, epic #251).
// SPEC: While the login page restores the session it shows a neutral
// spinner, not the full-screen BROOD brand panel with the launch splash: the
// splash flashed for a second on every reload of an authenticated panel.
import { describe, expect, test } from 'bun:test';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createInstance } from 'i18next';
import { I18nextProvider } from 'react-i18next';
import { MemoryRouter } from 'react-router-dom';
import { LoginPage } from '@/pages/LoginPage';
import en from '@/i18n/locales/en.json';

const i18n = createInstance();
await i18n.init({ lng: 'en', resources: { en: { translation: en } } });

// Первый рендер страницы входа — всегда восстановление сессии: autoLoading
// начинается с true, эффект ещё не отработал.
const restoring = () =>
  renderToStaticMarkup(
    createElement(
      I18nextProvider,
      { i18n },
      createElement(MemoryRouter, { initialEntries: ['/login'] }, createElement(LoginPage))
    )
  );

describe('session restore splash', () => {
  test('is a neutral status spinner', () => {
    const html = restoring();
    expect(html).toContain('role="status"');
    expect(html).toContain('loading-spinner');
  });

  test('shows neither the brand word nor the launch splash', () => {
    const html = restoring();
    expect(html).not.toContain('BROOD');
    expect(html).not.toContain('<h1');
    expect(html).not.toContain('<img');
    // Ключи сплэша ушли из локалей вместе с ним.
    expect('splash' in en).toBe(false);
  });

  test('has no login form yet', () => {
    expect(restoring()).not.toContain('<input');
  });
});

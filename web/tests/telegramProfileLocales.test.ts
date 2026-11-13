// FEATURE: Locale keys of the profile page and its Telegram section (#285).
// SPEC: Every `profile` key exists in all six locales with the
// interpolations of English; a counted phrase has every plural form the
// locale's plural rules use (Russian one/few/many/other, English one/other,
// Chinese, Korean and Vietnamese other) and no form beyond them and English's
// (Korean and Vietnamese carry every English key). Every key the source
// names exists, including each state and each notification kind it can show,
// and the header link to the profile is translated everywhere.
import { describe, expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { TELEGRAM_EVENTS } from '@/services/brood/telegram';

const locales = ['en', 'ru', 'zh-CN', 'zh-TW', 'ko', 'vi'];
const read = (locale: string) =>
  JSON.parse(readFileSync(new URL(`../src/i18n/locales/${locale}.json`, import.meta.url), 'utf8'));
const flatten = (value: unknown, prefix = ''): Record<string, string> => {
  if (!value || typeof value !== 'object' || Array.isArray(value)) {
    return prefix ? { [prefix]: String(value ?? '') } : {};
  }
  return Object.entries(value).reduce<Record<string, string>>(
    (leaves, [key, child]) => ({ ...leaves, ...flatten(child, prefix ? `${prefix}.${key}` : key) }),
    {}
  );
};
const tokens = (value: string) => (value.match(/\{\{[^}]+\}\}/g) ?? []).sort().join('|');
const PLURAL = /_(zero|one|two|few|many|other)$/;
const owned = (key: string) => key.startsWith('profile.');
const base = (key: string) => key.replace(PLURAL, '');

const english = flatten(read('en'));

describe('profile locale keys', () => {
  test('every locale carries every profile key with the English interpolations', () => {
    const keys = [...new Set(Object.keys(english).filter(owned).map(base))].sort();
    expect(keys.length).toBeGreaterThan(40);
    for (const locale of locales) {
      const translated = flatten(read(locale));
      const there = [...new Set(Object.keys(translated).filter(owned).map(base))].sort();
      expect({ locale, keys: there }).toEqual({ locale, keys });
      for (const key of Object.keys(translated).filter(owned)) {
        const reference = english[key] ?? english[`${base(key)}_other`];
        expect({ locale, key, filled: translated[key].trim().length > 0 }).toEqual({
          locale,
          key,
          filled: true,
        });
        expect({ locale, key, tokens: tokens(translated[key]) }).toEqual({
          locale,
          key,
          tokens: tokens(reference),
        });
      }
    }
  });

  test('a counted phrase has every plural form the locale uses, and no stray one', () => {
    const counted = [
      ...new Set(
        Object.keys(english)
          .filter((key) => owned(key) && PLURAL.test(key))
          .map(base)
      ),
    ];
    expect(counted).toContain('profile.telegram.minutes_left');
    const formsOf = (messages: Record<string, string>, phrase: string) =>
      Object.keys(messages)
        .filter((key) => base(key) === phrase && PLURAL.test(key))
        .map((key) => key.match(PLURAL)![1])
        .sort();
    for (const locale of locales) {
      const translated = flatten(read(locale));
      const used = [...new Intl.PluralRules(locale).resolvedOptions().pluralCategories];
      for (const phrase of counted) {
        const forms = formsOf(translated, phrase);
        // Формы английского допустимы везде: проверки корейской и
        // вьетнамской локалей требуют каждый ключ английской.
        const allowed = new Set([...used, ...formsOf(english, phrase)]);
        expect({ locale, phrase, missing: used.filter((form) => !forms.includes(form)) }).toEqual({
          locale,
          phrase,
          missing: [],
        });
        expect({ locale, phrase, stray: forms.filter((form) => !allowed.has(form)) }).toEqual({
          locale,
          phrase,
          stray: [],
        });
      }
    }
    expect(formsOf(flatten(read('ru')), 'profile.telegram.minutes_left')).toEqual([
      'few',
      'many',
      'one',
      'other',
    ]);
  });

  test('every key the page names exists, with each state and notification kind', () => {
    const files = [
      'features/profile/ProfilePage.tsx',
      'features/profile/ProfileLink.tsx',
      'features/profile/TelegramSection.tsx',
    ];
    const used = files.flatMap((file) =>
      [
        ...readFileSync(new URL(`../src/${file}`, import.meta.url), 'utf8').matchAll(
          /'((?:profile|header)\.[a-z_.]+)'/g
        ),
      ].map((match) => match[1])
    );
    expect(used.length).toBeGreaterThan(30);
    for (const key of used) {
      const known = key in english || `${key}_other` in english;
      expect({ key, known }).toEqual({ key, known: true });
    }
    for (const state of ['unlinked', 'linked', 'error', 'waiting']) {
      expect(english[`profile.telegram.state.${state}`]).toBeTruthy();
    }
    for (const kind of TELEGRAM_EVENTS) {
      expect(english[`profile.telegram.events.${kind}`]).toBeTruthy();
    }
  });

  test('the header link to the profile is translated in every locale', () => {
    for (const locale of locales) {
      const value = flatten(read(locale))['header.profile_of'];
      expect({ locale, value: tokens(value ?? '') }).toEqual({ locale, value: '{{user}}' });
    }
  });
});

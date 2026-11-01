// FEATURE: Актор-редактор без служебных идентификаторов.
// SPEC: ULID-подобный metadata.uid — внутренний идентификатор ateapi; человек
// адресует актора именем. Редактор не показывает сырой UID, имя остаётся.
// STATUS: RED (ожидает удаления строки UID из ActorEditor).
import { expect, test } from 'bun:test';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { I18nextProvider } from 'react-i18next';
import { createInstance } from 'i18next';
import type { Actor } from '@/services/brood/substrate';
import { ActorEditor } from '@/features/substrate/ActorEditor';
import en from '../src/i18n/locales/en.json';

const native: Actor = {
  metadata: { name: 'luna', uid: '01M6APGSFJMTQ2SJDX6WSX213C', version: '9' },
  provider: 'native',
  status: { state: 'ACTOR_STATE_RUNNING' },
} as unknown as Actor;

async function markup(actor: Actor) {
  const i18n = createInstance();
  await i18n.init({ lng: 'en', resources: { en: { translation: en } } });
  return renderToStaticMarkup(createElement(I18nextProvider, { i18n },
    createElement(ActorEditor, {
      project: 'swarm/a', actor, templates: [], open: true,
      onClose: () => {}, onSubmit: async () => true,
    })
  ));
}

test('actor status section keeps the name and hides the raw uid', async () => {
  const html = await markup(native);
  expect(html).toContain('luna');
  expect(html).not.toContain('01M6APGSFJMTQ2SJDX6WSX213C');
  expect(html).not.toContain('<dt>UID</dt>');
});

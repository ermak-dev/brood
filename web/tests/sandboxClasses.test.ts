import { describe, expect, test } from 'bun:test';
import {
  SANDBOX_CLASSES,
  missingSandboxClasses,
  newPoolDraft,
  validatePoolDraft,
  workerPoolImages,
} from '@/features/substrate/workerPoolDraft';

// FEATURE: Неустановленный класс песочницы виден с причиной, а не отсутствует (#91).
// SPEC: Поддерживаемые классы -- один список на всю форму. Установленным
// считается класс, для которого установка объявила образ; выбор образа
// остаётся за установкой, неявного включения класса нет.

const gvisor = `registry/gvisor@sha256:${'a'.repeat(64)}`;
const microvm = `registry/microvm@sha256:${'b'.repeat(64)}`;
const lxc = `registry/lxc@sha256:${'c'.repeat(64)}`;

describe('supported sandbox classes come from one list', () => {
  test('the order is the one the form offers, and validation shares it', () => {
    expect(SANDBOX_CLASSES).toEqual(['gvisor', 'microvm', 'lxc']);
    for (const sandboxClass of SANDBOX_CLASSES)
      expect(validatePoolDraft({ ...newPoolDraft(gvisor, sandboxClass), name: 'p' }).sandboxClass).toBeUndefined();
    expect(validatePoolDraft({ ...newPoolDraft(gvisor, 'fullvm'), name: 'p' })).toHaveProperty('sandboxClass');
  });
});

describe('a supported class without an installed image is named, not hidden', () => {
  test('only installed classes are offered', () => {
    expect(Object.keys(workerPoolImages({ workerImage: gvisor, workerImages: { gvisor, microvm } })))
      .toEqual(['gvisor', 'microvm']);
  });

  test('the ones left out are reported so the panel can explain them', () => {
    expect(missingSandboxClasses({ workerImage: gvisor, workerImages: { gvisor, microvm } })).toEqual(['lxc']);
    expect(missingSandboxClasses({ workerImage: gvisor, workerImages: { gvisor, microvm, lxc } })).toEqual([]);
    expect(missingSandboxClasses({ workerImage: gvisor })).toEqual(['microvm', 'lxc']);
  });

  test('an unsupported name is never reported as merely missing', () => {
    expect(missingSandboxClasses({ workerImage: gvisor, workerImages: { gvisor, microvm, lxc, fullvm: 'x' } }))
      .toEqual([]);
  });
});

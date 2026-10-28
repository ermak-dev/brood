import type { Binding, NativeIdentity } from '@/services/brood/substrate';

/** Каталог дополняется уже прочитанными членами политики: неизвестная
 * старому каталогу identity остаётся видимой и не теряется при сохранении. */
export function principalOptions(identities: NativeIdentity[] | null, bindings: Binding[]) {
  const options = new Map(
    (identities ?? []).map((identity) => [
      identity.id,
      { value: identity.id, label: identity.label },
    ])
  );
  for (const binding of bindings) {
    for (const member of binding.members ?? []) {
      if (!options.has(member))
        options.set(member, {
          value: member,
          label: member.replace(/^user:/, ''),
        });
    }
  }
  return [...options.values()];
}

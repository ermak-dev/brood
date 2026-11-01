import type { ProxmoxConnectionDraft } from '@/services/brood/substrate';
import { isShortName } from './format';

export interface ConnectionDraft extends Omit<ProxmoxConnectionDraft, 'capacity'> {
  template: string;
  templateDigest: string;
  actors: string;
  cpu: string;
  memoryGiB: string;
  diskGiB: string;
}

export function newConnectionDraft(): ConnectionDraft {
  return {
    name: '', endpoint: '', tokenID: '', tokenSecret: '', caPEM: '', node: '',
    resourcePool: '', storage: '', network: '', template: '', templateDigest: '',
    actors: '', cpu: '', memoryGiB: '', diskGiB: '',
  };
}

const integer = (value: string, max: number) =>
  /^[1-9][0-9]*$/.test(value) && Number.isSafeInteger(Number(value)) && Number(value) <= max;

export function validateConnectionDraft(draft: ConnectionDraft): Partial<Record<keyof ConnectionDraft, string>> {
  const errors: Partial<Record<keyof ConnectionDraft, string>> = {};
  for (const [key, value] of Object.entries(draft)) {
    if (!value.trim()) errors[key as keyof ConnectionDraft] = 'required';
  }
  if (!isShortName(draft.name) || draft.name.length > 48) errors.name = 'name';
  try {
    const url = new URL(draft.endpoint);
    if (url.protocol !== 'https:' || !url.hostname || url.username || url.password ||
      !['', '/'].includes(url.pathname) || /[?#\s]/.test(draft.endpoint)) errors.endpoint = 'origin';
  } catch { errors.endpoint = 'origin'; }
  if (!/^[A-Za-z0-9_.-]+@[A-Za-z0-9_.-]+![A-Za-z0-9_.-]+$/.test(draft.tokenID)) errors.tokenID = 'token';
  if (!/^[\x21-\x7e]+$/.test(draft.tokenSecret)) errors.tokenSecret = 'secret';
  if (!draft.caPEM.trim().startsWith('-----BEGIN CERTIFICATE-----') ||
    !draft.caPEM.trim().endsWith('-----END CERTIFICATE-----')) errors.caPEM = 'certificate';
  for (const key of ['node', 'resourcePool', 'storage', 'network', 'template'] as const) {
    if ([...draft[key]].some((char) => char.charCodeAt(0) < 32 || char.charCodeAt(0) === 127)) errors[key] = 'text';
  }
  if (!/^sha256:[a-f0-9]{64}$/.test(draft.templateDigest)) errors.templateDigest = 'digest';
  if (!integer(draft.actors, 1024)) errors.actors = 'integer';
  if (!integer(draft.cpu, 131072)) errors.cpu = 'integer';
  if (!integer(draft.memoryGiB, 1048576)) errors.memoryGiB = 'integer';
  if (!integer(draft.diskGiB, 16777216)) errors.diskGiB = 'integer';
  if (!errors.actors) {
    const actors = Number(draft.actors);
    if (!errors.cpu && Math.floor(Number(draft.cpu) / actors) < 1) errors.cpu = 'budget';
    if (!errors.memoryGiB && Math.floor(Number(draft.memoryGiB) * 1024 / actors) < 16) errors.memoryGiB = 'budget';
    if (!errors.diskGiB && Math.floor(Number(draft.diskGiB) / actors) < 1) errors.diskGiB = 'budget';
  }
  return errors;
}

export function connectionPayload(draft: ConnectionDraft): ProxmoxConnectionDraft {
  if (Object.keys(validateConnectionDraft(draft)).length) throw new Error('Invalid Proxmox pool configuration');
  const { actors, cpu, memoryGiB, diskGiB, ...connection } = draft;
  return {
    ...connection,
    capacity: {
      actors: Number(actors), cpuMillicores: String(Number(cpu) * 1000),
      memoryBytes: String(Number(memoryGiB) * 1073741824), diskGiB: Number(diskGiB),
    },
  };
}

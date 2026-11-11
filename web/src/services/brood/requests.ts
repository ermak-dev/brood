/**
 * brood: operator requests of a swarm (#209, API #210).
 *
 * GET /api/swarms/{id}/requests?all=1 lists every request of the swarm, open
 * and closed, for any swarm role. POST .../requests/{request}/answer takes
 * exactly {outcome?, response?} from a swarm manager and returns the request
 * after the answer; POST .../requests/{request}/deliver retries a failed or
 * unconfirmed delivery of the recorded answer to the asking actor (#260). A
 * delivery failure is not a refusal: the reply carries the delivery state
 * and its public reason (deliveryError). A node request routes
 * the graph by the outcome of one of its options; an actor request never
 * carries an outcome. Refusals are fixed public messages ({"detail": ...}).
 */

import { request } from './http';

/** Delivery of an answer to the asking actor (#260): `failed` surely did not
 * leave, so a retry is safe; `unconfirmed` left without a receipt and may
 * have reached the actor, so a retry may deliver it twice. */
export const DELIVERY_STATES = ['pending', 'delivered', 'failed', 'unconfirmed'] as const;

export type DeliveryState = (typeof DELIVERY_STATES)[number];

export const isDeliveryState = (value: string): value is DeliveryState =>
  (DELIVERY_STATES as readonly string[]).includes(value);

export interface RequestOption {
  label: string;
  outcome: string;
}

/** The asking actor; the name is empty once that actor no longer exists. */
export interface RequestActor {
  name: string;
  incarnation: number;
}

export interface SwarmRequest {
  id: string;
  swarmId: string;
  /** `node` (a waiting graph step) or `actor` (ask_operator). */
  origin: string;
  /** Step of a node request and the node it waits at. */
  sequence?: number;
  nodeId?: string;
  actor?: RequestActor;
  /** `choice`, `text` or `confirm`; a node request is always a choice. */
  kind: string;
  question: string;
  /** Material the question is about; empty when the asker sent none. */
  context: string;
  options: RequestOption[];
  /** `open`, `answered` or `cancelled`. */
  state: string;
  createdAt: string;
  answeredAt?: string;
  /** Username of the author of the answer. */
  answeredBy?: string;
  outcome?: string;
  /** Label of the option whose outcome was chosen. */
  optionLabel?: string;
  response?: string;
  /** Delivery of the answer to the asking actor, one of DELIVERY_STATES; a
   * newer server's state stays as sent and is shown as is. */
  delivery?: string;
  /** Public reason of a failed or unconfirmed delivery, only in an answer or
   * retry reply. */
  deliveryError?: string;
}

/** Exactly what the answer route accepts. */
export interface RequestAnswer {
  outcome?: string;
  response?: string;
}

const INVALID = 'Invalid swarm requests response';

const isRecord = (value: unknown): value is Record<string, unknown> =>
  Boolean(value) && typeof value === 'object' && !Array.isArray(value);

function optionalString(item: Record<string, unknown>, key: string): string | undefined {
  const value = item[key];
  if (value === undefined || value === null) return undefined;
  if (typeof value !== 'string') throw new Error(INVALID);
  return value;
}

function requiredString(item: Record<string, unknown>, key: string): string {
  const value = item[key];
  if (typeof value !== 'string' || (key !== 'context' && value === '')) throw new Error(INVALID);
  return value;
}

function parseOption(value: unknown): RequestOption {
  if (!isRecord(value) || typeof value.label !== 'string' || typeof value.outcome !== 'string') {
    throw new Error(INVALID);
  }
  return { label: value.label, outcome: value.outcome };
}

function parseActor(value: unknown): RequestActor | undefined {
  if (value === undefined || value === null) return undefined;
  if (!isRecord(value) || !Number.isSafeInteger(value.incarnation)) throw new Error(INVALID);
  const name = value.name ?? '';
  if (typeof name !== 'string') throw new Error(INVALID);
  return { name, incarnation: value.incarnation as number };
}

/** A request from the wire: every field the inbox shows is typed here, so a
 * malformed reply fails loudly instead of losing a question. */
export function parseSwarmRequest(value: unknown): SwarmRequest {
  if (!isRecord(value) || !Array.isArray(value.options)) throw new Error(INVALID);
  const sequence = value.sequence;
  if (sequence !== undefined && sequence !== null && !Number.isSafeInteger(sequence)) {
    throw new Error(INVALID);
  }
  const parsed: SwarmRequest = {
    id: requiredString(value, 'id'),
    swarmId: requiredString(value, 'swarmId'),
    origin: requiredString(value, 'origin'),
    kind: requiredString(value, 'kind'),
    question: requiredString(value, 'question'),
    context: optionalString(value, 'context') ?? '',
    options: value.options.map(parseOption),
    state: requiredString(value, 'state'),
    createdAt: requiredString(value, 'createdAt'),
  };
  if (typeof sequence === 'number') parsed.sequence = sequence;
  const actor = parseActor(value.actor);
  if (actor) parsed.actor = actor;
  for (const key of [
    'nodeId',
    'answeredAt',
    'answeredBy',
    'outcome',
    'optionLabel',
    'response',
    'delivery',
    'deliveryError',
  ] as const) {
    const field = optionalString(value, key);
    if (field !== undefined && field !== '') parsed[key] = field;
  }
  return parsed;
}

const segment = encodeURIComponent;
const requestsPath = (swarmId: string) => `/api/swarms/${segment(swarmId)}/requests`;
const requestPath = (swarmId: string, id: string) => `${requestsPath(swarmId)}/${segment(id)}`;

export const requestsApi = {
  /** Every request of the swarm: the inbox shows the open ones and the history. */
  list: async (swarmId: string): Promise<SwarmRequest[]> => {
    const body = await request<unknown>('GET', `${requestsPath(swarmId)}?all=1`);
    if (!isRecord(body) || !Array.isArray(body.results)) throw new Error(INVALID);
    return body.results.map(parseSwarmRequest);
  },
  answer: async (swarmId: string, id: string, answer: RequestAnswer): Promise<SwarmRequest> =>
    parseSwarmRequest(await request<unknown>('POST', `${requestPath(swarmId, id)}/answer`, answer)),
  deliver: async (swarmId: string, id: string): Promise<SwarmRequest> =>
    parseSwarmRequest(await request<unknown>('POST', `${requestPath(swarmId, id)}/deliver`)),
};

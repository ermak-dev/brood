import { describe, expect, test } from 'bun:test';
import { parseApiErrorResponse } from '../src/services/api/apiError';

describe('Management API error parsing', () => {
  test('prefers the human-readable message and preserves the API error code', () => {
    const result = parseApiErrorResponse(
      {
        error: 'plugin_install_failed',
        message: 'download plugin archive: 404 Not Found',
      },
      'Request failed with status code 502'
    );

    expect(result).toEqual({
      message: 'download plugin archive: 404 Not Found',
      apiCode: 'plugin_install_failed',
    });
  });

  test('falls back to a string error used by legacy endpoints', () => {
    expect(parseApiErrorResponse({ error: 'invalid body' }, 'Bad Request')).toEqual({
      message: 'invalid body',
      apiCode: 'invalid body',
    });
  });

  test('supports nested error messages and codes', () => {
    expect(
      parseApiErrorResponse(
        { error: { code: 'invalid_config', message: 'plugins-dir is invalid' } },
        'Bad Request'
      )
    ).toEqual({
      message: 'plugins-dir is invalid',
      apiCode: 'invalid_config',
    });
  });

  test('uses a text response body before the transport fallback', () => {
    expect(parseApiErrorResponse('upstream unavailable', 'Network Error')).toEqual({
      message: 'upstream unavailable',
    });
  });

  // Go API отвечает {"detail": ...}; текстовый ответ (config.yaml читается
  // как text) приходит строкой JSON, и показывать её сырой нельзя.
  test('reads the Go detail envelope, also when the body arrived as text', () => {
    const reason =
      'LLM backend is not configured: CLIPROXY_URL and (for management) CLIPROXY_MANAGEMENT_KEY are required';
    expect(parseApiErrorResponse({ detail: reason }, 'Request failed with status code 503')).toEqual(
      { message: reason, apiCode: undefined }
    );
    expect(
      parseApiErrorResponse(JSON.stringify({ detail: reason }), 'Request failed with status code 503')
    ).toEqual({ message: reason, apiCode: undefined });
    expect(parseApiErrorResponse('not json {', 'Network Error')).toEqual({
      message: 'not json {',
    });
  });

  test('uses the transport message for an unknown response shape', () => {
    expect(parseApiErrorResponse({ error: null }, 'Network Error')).toEqual({
      message: 'Network Error',
      apiCode: undefined,
    });
  });
});

import React, { useEffect, useMemo, useState, useCallback } from 'react';
import { Navigate, useLocation } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { Input } from '@/components/ui/Input';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import { Select } from '@/components/ui/Select';
import { IconEye, IconEyeOff } from '@/components/ui/icons';
import { useAuthStore, useLanguageStore, useNotificationStore } from '@/stores';
import { LANGUAGE_LABEL_KEYS, LANGUAGE_ORDER } from '@/utils/constants';
import { isSupportedLanguage } from '@/utils/language';
import { INLINE_LOGO } from '@/assets/logoInline';
import type { ApiError } from '@/types';
import { LegacyBackendError } from '@/services/api/legacyBackendProbe';
import { BroodAuthError } from '@/services/brood/session';
import { returnAddress } from '@/router/returnAddress';
import styles from './LoginPage.module.scss';

/**
 * 将 API 错误转换为本地化的用户友好消息
 */
function getLocalizedErrorMessage(error: unknown, t: (key: string) => string): string {
  if (error instanceof LegacyBackendError) return t('login.error_legacy_backend');
  // brood: a refusal of the brood sign-in itself, not of the management API.
  if (error instanceof BroodAuthError) {
    return error.status === 401 ? t('login.error_bad_credentials') : error.message;
  }
  const apiError = error as Partial<ApiError>;
  const status = typeof apiError.status === 'number' ? apiError.status : undefined;
  const code = typeof apiError.code === 'string' ? apiError.code : undefined;
  const message =
    error instanceof Error
      ? error.message
      : typeof apiError.message === 'string'
        ? apiError.message
        : typeof error === 'string'
          ? error
          : '';

  const withHttpStatus = (summary: string) => {
    if (!status) {
      return summary;
    }

    const genericAxiosMessage = `Request failed with status code ${status}`;
    const detail = message.trim();
    const backendDetail =
      detail && detail !== genericAxiosMessage
        ? ` (${t('login.error_backend_detail')}: ${detail})`
        : '';

    return `HTTP ${status}: ${summary}${backendDetail}`;
  };

  // 根据 HTTP 状态码判断
  if (status === 401) {
    return withHttpStatus(t('login.error_unauthorized'));
  }
  if (status === 403) {
    return withHttpStatus(t('login.error_forbidden'));
  }
  if (status === 404) {
    return withHttpStatus(t('login.error_not_found'));
  }
  if (status && status >= 500) {
    return withHttpStatus(t('login.error_server'));
  }

  // 根据 axios 错误码判断
  if (code === 'ECONNABORTED' || message.toLowerCase().includes('timeout')) {
    return t('login.error_timeout');
  }
  if (code === 'ERR_NETWORK' || message.toLowerCase().includes('network error')) {
    return t('login.error_network');
  }
  if (code === 'ERR_CERT_AUTHORITY_INVALID' || message.toLowerCase().includes('certificate')) {
    return t('login.error_ssl');
  }

  // 检查 CORS 错误
  if (message.toLowerCase().includes('cors') || message.toLowerCase().includes('cross-origin')) {
    return t('login.error_cors');
  }

  // 默认错误消息
  return withHttpStatus(t('login.error_invalid'));
}

export function LoginPage() {
  const { t } = useTranslation();
  const location = useLocation();
  const { showNotification } = useNotificationStore();
  const language = useLanguageStore((state) => state.language);
  const setLanguage = useLanguageStore((state) => state.setLanguage);
  const isAuthenticated = useAuthStore((state) => state.isAuthenticated);
  const login = useAuthStore((state) => state.login);
  const restoreSession = useAuthStore((state) => state.restoreSession);
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [showKey, setShowKey] = useState(false);
  const [loading, setLoading] = useState(false);
  const [autoLoading, setAutoLoading] = useState(true);
  const [error, setError] = useState('');

  const languageOptions = useMemo(
    () =>
      LANGUAGE_ORDER.map((lang) => ({
        value: lang,
        label: t(LANGUAGE_LABEL_KEYS[lang]),
      })),
    [t]
  );
  const handleLanguageChange = useCallback(
    (selectedLanguage: string) => {
      if (!isSupportedLanguage(selectedLanguage)) {
        return;
      }
      setLanguage(selectedLanguage);
    },
    [setLanguage]
  );

  // brood (#256): восстановление сессии — не событие. Раньше его сопровождал
  // полноэкранный сплэш «BROOD» с задержкой 1,5 с на каждой перезагрузке;
  // теперь восстановленная сессия сразу уводит на запрошенный адрес (Navigate
  // ниже), а до ответа сервера виден нейтральный спиннер.
  useEffect(() => {
    const init = async () => {
      try {
        await restoreSession();
      } finally {
        setAutoLoading(false);
      }
    };

    init();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const handleSubmit = useCallback(async () => {
    if (!username.trim() || !password) {
      setError(t('login.error_credentials_required'));
      return;
    }

    setLoading(true);
    setError('');
    try {
      await login({ username, password });
      // Адрес после входа выбирает переход ниже (isAuthenticated), как после
      // восстановления; прежний navigate('/') здесь шёл за ним и уводил на
      // корень, участника — к списку проектов (#240, #262).
      showNotification(t('common.connected_status'), 'success');
    } catch (err: unknown) {
      const message = getLocalizedErrorMessage(err, t);
      setError(message);
      showNotification(`${t('notification.login_failed')}: ${message}`, 'error');
    } finally {
      setLoading(false);
    }
  }, [login, password, showNotification, t, username]);

  const handleSubmitKeyDown = useCallback(
    (event: React.KeyboardEvent) => {
      if (event.key === 'Enter' && !loading) {
        event.preventDefault();
        handleSubmit();
      }
    },
    [loading, handleSubmit]
  );

  if (isAuthenticated && !autoLoading) {
    // #262: весь адрес, с которого увёл ProtectedRoute, с query и hash.
    return <Navigate to={returnAddress(location.state)} replace />;
  }

  if (autoLoading) {
    return (
      <div className={styles.restoring}>
        <LoadingSpinner size={28} />
      </div>
    );
  }

  return (
    <div className={styles.container}>
      {/* 左侧品牌展示区 */}
      <div className={styles.brandPanel}>
        <div className={styles.brandContent}>
          <span className={styles.brandWord}>BROOD</span>
        </div>
      </div>

      {/* 右侧功能交互区 */}
      <div className={styles.formPanel}>
        {/* 登录表单 */}
        <div className={styles.formContent}>
          {/* Logo */}
          <img src={INLINE_LOGO} alt="Logo" className={styles.logo} />

          {/* 登录表单卡片 */}
          <div className={styles.loginCard}>
            <div className={styles.loginHeader}>
              <div className={styles.titleRow}>
                <div className={styles.title}>{t('title.login')}</div>
                <Select
                  className={styles.languageSelect}
                  value={language}
                  options={languageOptions}
                  onChange={handleLanguageChange}
                  fullWidth={false}
                  ariaLabel={t('language.switch')}
                />
              </div>
            </div>

            <Input
              autoFocus
              label={t('login.username_label')}
              placeholder={t('login.username_placeholder')}
              name="username"
              autoComplete="username"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              onKeyDown={handleSubmitKeyDown}
            />

            <Input
              label={t('login.password_label')}
              placeholder={t('login.password_placeholder')}
              type={showKey ? 'text' : 'password'}
              name="password"
              autoComplete="current-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              onKeyDown={handleSubmitKeyDown}
              rightElement={
                <button
                  type="button"
                  className="btn btn-ghost btn-sm"
                  onClick={() => setShowKey((prev) => !prev)}
                  aria-label={showKey ? t('login.hide_password') : t('login.show_password')}
                  title={showKey ? t('login.hide_password') : t('login.show_password')}
                >
                  {showKey ? <IconEyeOff size={16} /> : <IconEye size={16} />}
                </button>
              }
            />

            <Button fullWidth onClick={handleSubmit} loading={loading}>
              {loading ? t('login.submitting') : t('login.submit_button')}
            </Button>

            {error && <div className={styles.errorBox}>{error}</div>}
          </div>
        </div>
      </div>
    </div>
  );
}

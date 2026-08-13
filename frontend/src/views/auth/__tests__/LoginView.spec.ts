import { flushPromises, mount, RouterLinkStub } from '@vue/test-utils'
import { ref } from 'vue'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import LoginView from '@/views/auth/LoginView.vue'

const { getPublicSettingsMock, pushMock, routerState } = vi.hoisted(() => ({
  getPublicSettingsMock: vi.fn(),
  pushMock: vi.fn(),
  routerState: { currentRoute: { value: { query: {} as Record<string, string> } } }
}))

const publicSettings = {
  registration_enabled: true,
  turnstile_enabled: false,
  turnstile_site_key: '',
  tencent_captcha_enabled: false,
  tencent_captcha_app_id: '',
  aliyun_captcha_enabled: false,
  aliyun_captcha_scene_id: '',
  aliyun_captcha_prefix: '',
  linuxdo_oauth_enabled: false,
  dingtalk_oauth_enabled: false,
  wechat_oauth_enabled: false,
  backend_mode_enabled: false,
  oidc_oauth_enabled: false,
  oidc_oauth_provider_name: 'OIDC',
  github_oauth_enabled: false,
  google_oauth_enabled: false,
  password_reset_enabled: false,
  passkey_enabled: false,
  login_agreement_enabled: false,
  login_agreement_documents: []
}

vi.mock('vue-router', () => ({
  useRouter: () => ({
    push: pushMock,
    currentRoute: routerState.currentRoute
  })
}))

vi.mock('vue-i18n', () => ({
  createI18n: () => ({
    global: {
      t: (key: string) => key
    }
  }),
  useI18n: () => ({
    t: (key: string) => key
  })
}))

vi.mock('@/stores', () => ({
  useAuthStore: () => ({
    login: vi.fn(),
    loginWithPasskey: vi.fn(),
    login2FA: vi.fn()
  }),
  useAppStore: () => ({
    showError: vi.fn(),
    showSuccess: vi.fn(),
    showWarning: vi.fn()
  })
}))

vi.mock('@/api/auth', () => ({
  buildOAuthLoginStartURL: vi.fn(),
  getPublicSettings: (...args: unknown[]) => getPublicSettingsMock(...args),
  isTotp2FARequired: vi.fn(() => false),
  isWeChatWebOAuthEnabled: vi.fn(() => false),
  startOAuthLogin: vi.fn()
}))

function mountLogin() {
  return mount(LoginView, {
    global: {
      stubs: {
        AuthLayout: { template: '<div><slot /><slot name="footer" /></div>' },
        DingTalkOAuthSection: true,
        EmailOAuthButtons: true,
        Icon: true,
        LinuxDoOAuthSection: true,
        LoginAgreementPrompt: true,
        OidcOAuthSection: true,
        RouterLink: RouterLinkStub,
        TotpLoginModal: true,
        TurnstileWidget: true,
        WechatOAuthSection: true,
        transition: false
      }
    }
  })
}

describe('LoginView registration entry', () => {
  beforeEach(() => {
    getPublicSettingsMock.mockReset()
    pushMock.mockReset()
    routerState.currentRoute = ref({ query: {} as Record<string, string> })
    getPublicSettingsMock.mockResolvedValue(publicSettings)
  })

  it('shows the registration entry when registration is enabled', async () => {
    const wrapper = mountLogin()
    await flushPromises()

    expect(wrapper.text()).toContain('auth.signUp')
  })

  it('hides the registration entry when registration is disabled', async () => {
    getPublicSettingsMock.mockResolvedValueOnce({
      ...publicSettings,
      registration_enabled: false
    })

    const wrapper = mountLogin()
    await flushPromises()

    expect(wrapper.text()).not.toContain('auth.signUp')
  })

  it('shows XSci SSO as primary and preserves query parameters in the email entry', async () => {
    routerState.currentRoute.value.query = { redirect: '/usage' }
    getPublicSettingsMock.mockResolvedValueOnce({
      ...publicSettings,
      oidc_oauth_enabled: true,
      oidc_oauth_provider_name: 'XSci'
    })

    const wrapper = mountLogin()
    await flushPromises()

    expect(wrapper.find('form').exists()).toBe(false)
    expect(wrapper.getComponent({ name: 'OidcOAuthSection' }).props('variant')).toBe('primary')
    expect(wrapper.text()).not.toContain('auth.signUp')
    const emailEntry = wrapper.getComponent('[data-testid="email-login-entry"]')
    expect(emailEntry.text()).toContain('auth.emailSignIn')
    expect(emailEntry.props('to')).toEqual({
      path: '/login',
      query: { redirect: '/usage', login: 'email' }
    })

    routerState.currentRoute.value.query = emailEntry.props('to').query
    await flushPromises()

    expect(wrapper.find('form').exists()).toBe(true)
    expect(wrapper.find('#email').exists()).toBe(true)
    expect(wrapper.find('[data-testid="email-login-entry"]').exists()).toBe(false)
  })

  it('keeps the full email form and hides registration in XSci email mode when disabled', async () => {
    routerState.currentRoute.value.query = { login: 'email' }
    getPublicSettingsMock.mockResolvedValueOnce({
      ...publicSettings,
      registration_enabled: false,
      oidc_oauth_enabled: true,
      oidc_oauth_provider_name: 'XSci',
      password_reset_enabled: true
    })

    const wrapper = mountLogin()
    await flushPromises()

    expect(wrapper.get('form #email').attributes('type')).toBe('email')
    expect(wrapper.get('form #password').attributes('type')).toBe('password')
    expect(wrapper.get('form button[type="submit"]').text()).toContain('auth.signIn')
    expect(wrapper.text()).toContain('auth.forgotPassword')
    expect(wrapper.findComponent({ name: 'OidcOAuthSection' }).exists()).toBe(true)
    expect(wrapper.find('[data-testid="email-login-entry"]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('auth.signUp')
  })
})

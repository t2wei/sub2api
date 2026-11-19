import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import LLMLoggingSettingCard from '../LLMLoggingSettingCard.vue'

const mocks = vi.hoisted(() => ({
  getSetting: vi.fn(),
  updateSetting: vi.fn(),
  showSuccess: vi.fn(),
  showError: vi.fn()
}))

vi.mock('@/api', () => ({
  adminAPI: {
    settings: {
      getLLMLoggingSetting: mocks.getSetting,
      updateLLMLoggingSetting: mocks.updateSetting
    }
  }
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key })
}))

vi.mock('@/stores', () => ({
  useAppStore: () => ({
    showSuccess: mocks.showSuccess,
    showError: mocks.showError
  })
}))

const wrappers: ReturnType<typeof mount>[] = []

function mountCard() {
  const wrapper = mount(LLMLoggingSettingCard, { attachTo: document.body })
  wrappers.push(wrapper)
  return wrapper
}

afterEach(() => wrappers.splice(0).forEach((wrapper) => wrapper.unmount()))

beforeEach(() => {
  vi.clearAllMocks()
})

describe('LLMLoggingSettingCard', () => {
  it('shows the switch off and keeps save disabled until it is changed', async () => {
    mocks.getSetting.mockResolvedValue({ enabled: false, sink_configured: true })
    const wrapper = mountCard()
    await flushPromises()

    expect(wrapper.get('[data-testid="llm-logging-toggle"]').attributes('aria-checked')).toBe('false')
    expect(wrapper.get('[data-testid="llm-logging-save"]').attributes('disabled')).toBeDefined()
    expect(wrapper.find('[data-testid="llm-logging-sink-missing"]').exists()).toBe(false)
  })

  it('turns logging on and saves', async () => {
    mocks.getSetting.mockResolvedValue({ enabled: false, sink_configured: true })
    mocks.updateSetting.mockResolvedValue({ enabled: true, sink_configured: true })
    const wrapper = mountCard()
    await flushPromises()

    await wrapper.get('[data-testid="llm-logging-toggle"]').trigger('click')
    await wrapper.get('[data-testid="llm-logging-save"]').trigger('click')
    await flushPromises()

    expect(mocks.updateSetting).toHaveBeenCalledWith(true)
    expect(mocks.showSuccess).toHaveBeenCalledWith('admin.settings.llmLogging.saved')
    expect(wrapper.get('[data-testid="llm-logging-toggle"]').attributes('aria-checked')).toBe('true')
    expect(wrapper.get('[data-testid="llm-logging-save"]').attributes('disabled')).toBeDefined()
  })

  it('warns when no sink is configured', async () => {
    mocks.getSetting.mockResolvedValue({ enabled: false, sink_configured: false })
    const wrapper = mountCard()
    await flushPromises()

    expect(wrapper.find('[data-testid="llm-logging-sink-missing"]').exists()).toBe(true)
  })

  it('reports a refused save and keeps the change', async () => {
    mocks.getSetting.mockResolvedValue({ enabled: false, sink_configured: true })
    mocks.updateSetting.mockRejectedValue(new Error('boom'))
    const wrapper = mountCard()
    await flushPromises()

    await wrapper.get('[data-testid="llm-logging-toggle"]').trigger('click')
    await wrapper.get('[data-testid="llm-logging-save"]').trigger('click')
    await flushPromises()

    expect(mocks.showError).toHaveBeenCalled()
    expect(wrapper.get('[data-testid="llm-logging-toggle"]').attributes('aria-checked')).toBe('true')
  })

  it('offers a retry when loading fails', async () => {
    mocks.getSetting
      .mockRejectedValueOnce(new Error('boom'))
      .mockResolvedValueOnce({ enabled: false, sink_configured: true })
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {})
    const wrapper = mountCard()
    await flushPromises()

    expect(wrapper.find('[data-testid="llm-logging-load-error"]').exists()).toBe(true)
    await wrapper.get('[data-testid="llm-logging-load-error"] button').trigger('click')
    await flushPromises()
    expect(wrapper.find('[data-testid="llm-logging-toggle"]').exists()).toBe(true)
    errorSpy.mockRestore()
  })
})

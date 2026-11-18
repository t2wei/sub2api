import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import OIDCRequiredGroupsEditor from '../OIDCRequiredGroupsEditor.vue'

const mocks = vi.hoisted(() => ({
  getGroups: vi.fn(),
  updateGroups: vi.fn(),
  showSuccess: vi.fn(),
  showError: vi.fn()
}))

vi.mock('@/api', () => ({
  adminAPI: {
    settings: {
      getOIDCRequiredGroups: mocks.getGroups,
      updateOIDCRequiredGroups: mocks.updateGroups
    }
  }
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string, params?: Record<string, unknown>) =>
      params ? `${key}:${JSON.stringify(params)}` : key
  })
}))

vi.mock('@/stores', () => ({
  useAppStore: () => ({
    showSuccess: mocks.showSuccess,
    showError: mocks.showError
  })
}))

const wrappers: ReturnType<typeof mount>[] = []

function mountEditor() {
  const wrapper = mount(OIDCRequiredGroupsEditor, { attachTo: document.body })
  wrappers.push(wrapper)
  return wrapper
}

afterEach(() => wrappers.splice(0).forEach((wrapper) => wrapper.unmount()))

beforeEach(() => {
  vi.clearAllMocks()
})

describe('OIDCRequiredGroupsEditor', () => {
  it('shows "no requirement" when nothing is saved and keeps save disabled until edited', async () => {
    mocks.getGroups.mockResolvedValue({ groups: [], max: 50 })
    const wrapper = mountEditor()
    await flushPromises()

    expect(wrapper.get('[data-testid="oidc-required-groups-status"]').text()).toBe(
      'admin.settings.oidc.requiredGroups.statusOpen'
    )
    expect(wrapper.get('[data-testid="oidc-required-groups-save"]').attributes('disabled')).toBeDefined()
  })

  it('loads the saved groups one per line and saves the edited list', async () => {
    mocks.getGroups.mockResolvedValue({ groups: ['staff.*'], max: 50 })
    mocks.updateGroups.mockResolvedValue({ groups: ['staff.*', 'admins'], max: 50 })
    const wrapper = mountEditor()
    await flushPromises()

    const input = wrapper.get('[data-testid="oidc-required-groups-input"]')
    expect((input.element as HTMLTextAreaElement).value).toBe('staff.*')
    expect(wrapper.get('[data-testid="oidc-required-groups-status"]').text()).toContain('staff.*')

    await input.setValue('staff.*\n\n admins , ')
    await wrapper.get('[data-testid="oidc-required-groups-save"]').trigger('click')
    await flushPromises()

    expect(mocks.updateGroups).toHaveBeenCalledWith(['staff.*', 'admins'])
    expect(mocks.showSuccess).toHaveBeenCalledWith('admin.settings.oidc.requiredGroups.saved')
    expect((input.element as HTMLTextAreaElement).value).toBe('staff.*\nadmins')
  })

  it('clearing the box saves an empty list (no requirement)', async () => {
    mocks.getGroups.mockResolvedValue({ groups: ['staff.*'], max: 50 })
    mocks.updateGroups.mockResolvedValue({ groups: [], max: 50 })
    const wrapper = mountEditor()
    await flushPromises()

    await wrapper.get('[data-testid="oidc-required-groups-input"]').setValue('')
    await wrapper.get('[data-testid="oidc-required-groups-save"]').trigger('click')
    await flushPromises()

    expect(mocks.updateGroups).toHaveBeenCalledWith([])
    expect(wrapper.get('[data-testid="oidc-required-groups-status"]').text()).toBe(
      'admin.settings.oidc.requiredGroups.statusOpen'
    )
  })

  it('reports a refused save and keeps the draft', async () => {
    mocks.getGroups.mockResolvedValue({ groups: [], max: 50 })
    mocks.updateGroups.mockRejectedValue(new Error('invalid group entry (1): st*aff'))
    const wrapper = mountEditor()
    await flushPromises()

    const input = wrapper.get('[data-testid="oidc-required-groups-input"]')
    await input.setValue('st*aff')
    await wrapper.get('[data-testid="oidc-required-groups-save"]').trigger('click')
    await flushPromises()

    expect(mocks.showError).toHaveBeenCalled()
    expect((input.element as HTMLTextAreaElement).value).toBe('st*aff')
  })

  it('offers a retry when loading fails', async () => {
    mocks.getGroups.mockRejectedValueOnce(new Error('boom')).mockResolvedValueOnce({ groups: [], max: 50 })
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {})
    const wrapper = mountEditor()
    await flushPromises()

    expect(wrapper.find('[data-testid="oidc-required-groups-load-error"]').exists()).toBe(true)
    await wrapper.get('[data-testid="oidc-required-groups-load-error"] button').trigger('click')
    await flushPromises()
    expect(wrapper.find('[data-testid="oidc-required-groups-input"]').exists()).toBe(true)
    errorSpy.mockRestore()
  })
})

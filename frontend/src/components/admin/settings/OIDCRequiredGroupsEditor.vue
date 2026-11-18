<template>
  <div
    class="space-y-3 rounded border border-gray-200 px-4 py-3 dark:border-dark-700"
    data-testid="oidc-required-groups-editor"
  >
    <div>
      <label class="font-medium text-gray-900 dark:text-white" for="oidc-required-groups-input">
        {{ t('admin.settings.oidc.requiredGroups.title') }}
      </label>
      <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
        {{ t('admin.settings.oidc.requiredGroups.hint') }}
      </p>
    </div>

    <p v-if="loading" class="text-sm text-gray-500 dark:text-gray-400">{{ t('common.loading') }}</p>
    <p v-else-if="loadError" class="text-sm text-red-600 dark:text-red-400" data-testid="oidc-required-groups-load-error">
      {{ t('admin.settings.oidc.requiredGroups.loadFailed') }}
      <button type="button" class="ml-2 underline" @click="load">
        {{ t('admin.settings.oidc.requiredGroups.retry') }}
      </button>
    </p>
    <template v-else>
      <textarea
        id="oidc-required-groups-input"
        v-model="draft"
        rows="3"
        class="input w-full font-mono text-sm"
        :placeholder="t('admin.settings.oidc.requiredGroups.placeholder')"
        data-testid="oidc-required-groups-input"
      />
      <div class="flex flex-wrap items-center justify-between gap-3">
        <span class="text-sm text-gray-700 dark:text-gray-300" data-testid="oidc-required-groups-status">
          {{
            saved.length === 0
              ? t('admin.settings.oidc.requiredGroups.statusOpen')
              : t('admin.settings.oidc.requiredGroups.statusRestricted', { groups: saved.join(', ') })
          }}
        </span>
        <button
          type="button"
          class="btn btn-primary btn-sm"
          :disabled="saving || !dirty"
          data-testid="oidc-required-groups-save"
          @click="save"
        >
          {{ saving ? t('common.saving') : t('common.save') }}
        </button>
      </div>
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api'
import { useAppStore } from '@/stores'
import { extractApiErrorMessage } from '@/utils/apiError'

const { t } = useI18n()
const appStore = useAppStore()

const loading = ref(true)
const loadError = ref(false)
const saving = ref(false)
const saved = ref<string[]>([])
const draft = ref('')

// One entry per line (commas also split); blank lines are ignored. The backend
// validates and normalises; this only shapes the request.
function parseDraft(value: string): string[] {
  return value
    .split(/[\n,]/)
    .map((item) => item.trim())
    .filter((item) => item !== '')
}

const dirty = computed(() => parseDraft(draft.value).join('\n') !== saved.value.join('\n'))

function apply(groups: string[] | null | undefined) {
  saved.value = groups ?? []
  draft.value = saved.value.join('\n')
}

async function load() {
  loading.value = true
  loadError.value = false
  try {
    const result = await adminAPI.settings.getOIDCRequiredGroups()
    apply(result.groups)
  } catch (error) {
    loadError.value = true
    console.error('Failed to load OIDC required groups:', error)
  } finally {
    loading.value = false
  }
}

async function save() {
  saving.value = true
  try {
    const result = await adminAPI.settings.updateOIDCRequiredGroups(parseDraft(draft.value))
    apply(result.groups)
    appStore.showSuccess(t('admin.settings.oidc.requiredGroups.saved'))
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, t('common.error')))
  } finally {
    saving.value = false
  }
}

onMounted(load)
</script>

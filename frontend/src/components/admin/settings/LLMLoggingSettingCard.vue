<template>
  <!-- [OXSCI] Call logging to data-service: runtime switch, own endpoint, saved on its own -->
  <div class="card" data-testid="llm-logging-setting">
    <div class="border-b border-gray-100 px-6 py-4 dark:border-dark-700">
      <h2 class="text-lg font-semibold text-gray-900 dark:text-white">
        {{ t('admin.settings.llmLogging.title') }}
      </h2>
      <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
        {{ t('admin.settings.llmLogging.description') }}
      </p>
    </div>
    <div class="space-y-5 p-6">
      <p v-if="loading" class="text-sm text-gray-500 dark:text-gray-400">{{ t('common.loading') }}</p>
      <p v-else-if="loadError" class="text-sm text-red-600 dark:text-red-400" data-testid="llm-logging-load-error">
        {{ t('admin.settings.llmLogging.loadFailed') }}
        <button type="button" class="ml-2 underline" @click="load">
          {{ t('admin.settings.llmLogging.retry') }}
        </button>
      </p>
      <template v-else>
        <div class="flex items-center justify-between">
          <div>
            <label class="font-medium text-gray-900 dark:text-white">
              {{ t('admin.settings.llmLogging.enabled') }}
            </label>
            <p class="text-sm text-gray-500 dark:text-gray-400">
              {{ t('admin.settings.llmLogging.enabledHint') }}
            </p>
          </div>
          <Toggle v-model="draft" data-testid="llm-logging-toggle" />
        </div>
        <p
          v-if="!sinkConfigured"
          class="text-sm text-amber-600 dark:text-amber-400"
          data-testid="llm-logging-sink-missing"
        >
          {{ t('admin.settings.llmLogging.sinkMissing') }}
        </p>
        <div class="flex justify-end border-t border-gray-100 pt-4 dark:border-dark-700">
          <button
            type="button"
            class="btn btn-primary btn-sm"
            :disabled="saving || draft === saved"
            data-testid="llm-logging-save"
            @click="save"
          >
            {{ saving ? t('common.saving') : t('common.save') }}
          </button>
        </div>
      </template>
    </div>
  </div>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api'
import { useAppStore } from '@/stores'
import { extractApiErrorMessage } from '@/utils/apiError'
import Toggle from '@/components/common/Toggle.vue'

const { t } = useI18n()
const appStore = useAppStore()

const loading = ref(true)
const loadError = ref(false)
const saving = ref(false)
const saved = ref(false)
const draft = ref(false)
const sinkConfigured = ref(true)

function apply(result: { enabled: boolean; sink_configured: boolean }) {
  saved.value = result.enabled
  draft.value = result.enabled
  sinkConfigured.value = result.sink_configured
}

async function load() {
  loading.value = true
  loadError.value = false
  try {
    apply(await adminAPI.settings.getLLMLoggingSetting())
  } catch (error) {
    loadError.value = true
    console.error('Failed to load the call logging setting:', error)
  } finally {
    loading.value = false
  }
}

async function save() {
  saving.value = true
  try {
    apply(await adminAPI.settings.updateLLMLoggingSetting(draft.value))
    appStore.showSuccess(t('admin.settings.llmLogging.saved'))
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, t('common.error')))
  } finally {
    saving.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="space-y-4">
    <button type="button" :disabled="disabled" class="btn w-full" :class="buttonClass" @click="startLogin">
      <span
        class="mr-2 inline-flex h-5 w-5 items-center justify-center rounded-full text-xs font-semibold"
        :class="providerInitialClass"
      >
        {{ providerInitial }}
      </span>
      {{ t('auth.oidc.signIn', { providerName: normalizedProviderName }) }}
    </button>

    <div v-if="showDivider" class="flex items-center gap-3">
      <div class="h-px flex-1 bg-gray-200 dark:bg-dark-700"></div>
      <span class="text-xs text-gray-500 dark:text-dark-400">
        {{ t('auth.oauthOrContinue') }}
      </span>
      <div class="h-px flex-1 bg-gray-200 dark:bg-dark-700"></div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'
import type { OAuthLoginStart } from '@/api/auth'
import { resolveAffiliateReferralCode, storeOAuthAffiliateCode } from '@/utils/oauthAffiliate'
import { normalizeOIDCProviderName } from '@/utils/oidcProviderName'

const props = withDefaults(defineProps<{
  disabled?: boolean
  affCode?: string
  providerName?: string
  showDivider?: boolean
  variant?: 'primary' | 'secondary'
}>(), {
  providerName: 'OIDC',
  showDivider: true,
  variant: 'secondary'
})
const emit = defineEmits<{
  start: [request: OAuthLoginStart]
}>()

const route = useRoute()
const { t } = useI18n()

const normalizedProviderName = computed(() => {
  return normalizeOIDCProviderName(props.providerName)
})

const providerInitial = computed(() => normalizedProviderName.value.charAt(0).toUpperCase() || 'O')
const buttonClass = computed(() => (props.variant === 'primary' ? 'btn-primary' : 'btn-secondary'))
const providerInitialClass = computed(() =>
  props.variant === 'primary'
    ? 'bg-white/20 text-white'
    : 'bg-primary-100 text-primary-700 dark:bg-primary-900/30 dark:text-primary-300'
)

function startLogin(): void {
  const redirectTo = (route.query.redirect as string) || '/dashboard'
  storeOAuthAffiliateCode(resolveAffiliateReferralCode(props.affCode, route.query.aff, route.query.aff_code))
  emit('start', { provider: 'oidc', params: { redirect: redirectTo } })
}
</script>

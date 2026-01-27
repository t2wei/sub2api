<template>
  <div class="min-h-screen bg-gray-50 flex items-center justify-center px-4 dark:bg-dark-900">
    <div class="text-center">
      <div v-if="loading" class="space-y-4">
        <div class="animate-spin rounded-full h-12 w-12 border-b-2 border-primary-600 mx-auto"></div>
        <p class="text-gray-600 dark:text-gray-400">{{ t('auth.oauth.processing') }}</p>
      </div>

      <div v-else-if="error" class="space-y-4">
        <div class="rounded-full h-12 w-12 bg-red-100 dark:bg-red-900/30 flex items-center justify-center mx-auto">
          <svg class="h-6 w-6 text-red-600 dark:text-red-400" fill="none" viewBox="0 0 24 24" stroke="currentColor">
            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M6 18L18 6M6 6l12 12" />
          </svg>
        </div>
        <div>
          <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ t('auth.oauth.failed') }}</h2>
          <p class="mt-1 text-sm text-red-600 dark:text-red-400">{{ error }}</p>
        </div>
        <router-link to="/login" class="btn btn-primary">
          {{ t('auth.oauth.backToLogin') }}
        </router-link>
      </div>

      <div v-else class="space-y-4">
        <div class="rounded-full h-12 w-12 bg-green-100 dark:bg-green-900/30 flex items-center justify-center mx-auto">
          <svg class="h-6 w-6 text-green-600 dark:text-green-400" fill="none" viewBox="0 0 24 24" stroke="currentColor">
            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M5 13l4 4L19 7" />
          </svg>
        </div>
        <div>
          <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ t('auth.oauth.success') }}</h2>
          <p class="mt-1 text-sm text-gray-600 dark:text-gray-400">{{ t('auth.oauth.redirecting') }}</p>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useAuthStore } from '@/stores/auth'

const router = useRouter()
const { t } = useI18n()
const authStore = useAuthStore()

const loading = ref(true)
const error = ref('')

onMounted(async () => {
  try {
    // 从 URL fragment 中提取参数 (格式: #access_token=xxx&token_type=Bearer&redirect=/dashboard)
    const hash = window.location.hash.substring(1)
    const params = new URLSearchParams(hash)

    const accessToken = params.get('access_token')
    const errorCode = params.get('error')
    const errorMessage = params.get('error_message') || params.get('error_description')
    // 解码 redirect 路径（可能被 URL 编码）
    const rawRedirect = params.get('redirect') || '/dashboard'
    const redirectPath = decodeURIComponent(rawRedirect)

    if (errorCode) {
      error.value = errorMessage || errorCode
      loading.value = false
      return
    }

    if (!accessToken) {
      error.value = t('auth.oauth.noToken')
      loading.value = false
      return
    }

    // 保存 token 并设置登录状态（setToken 内部会自动获取用户信息）
    await authStore.setToken(accessToken)

    loading.value = false

    // 短暂显示成功状态后跳转
    setTimeout(() => {
      router.push(redirectPath)
    }, 500)
  } catch (err: any) {
    error.value = err.message || t('auth.oauth.unknownError')
    loading.value = false
  }
})
</script>

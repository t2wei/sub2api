export const XSCI_OIDC_PROVIDER_NAME = 'XSci'

export function normalizeOIDCProviderName(providerName?: string | null): string {
  const name = providerName?.trim()
  if (!name) {
    return XSCI_OIDC_PROVIDER_NAME
  }
  const normalized = name.toLowerCase()
  if (normalized === 'oidc' || normalized === 'oxsci' || normalized === 'xsci') {
    return XSCI_OIDC_PROVIDER_NAME
  }
  return name
}

export function isXSciOIDCProvider(providerName?: string | null): boolean {
  return normalizeOIDCProviderName(providerName) === XSCI_OIDC_PROVIDER_NAME
}

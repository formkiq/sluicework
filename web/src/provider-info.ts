export const cloudSetup: Record<string, { name: string; keys: string; hint: string }> = {
  openai: { name: 'OpenAI', keys: 'https://platform.openai.com/api-keys', hint: 'API usage is billed separately from a ChatGPT subscription.' },
  anthropic: { name: 'Claude', keys: 'https://console.anthropic.com/settings/keys', hint: 'Claude subscriptions cannot authenticate third-party model requests. Use a Claude API key.' },
  gemini: { name: 'Gemini', keys: 'https://aistudio.google.com/apikey', hint: 'Google AI subscriptions do not provide a general third-party API login. Use a Gemini API key.' },
}

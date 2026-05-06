import { createApp } from 'vue'
import ElementPlus from 'element-plus'
import 'element-plus/dist/index.css'
import * as ElementPlusIconsVue from '@element-plus/icons-vue'
import App from './App.vue'
import axios from 'axios'
import { getApiKey, getAdminKey } from './config.js'

axios.interceptors.request.use(config => {
  if (config.url === '/api/verify-key' || config.url === '/api/health') {
    return config
  }

  if (config.headers['X-API-Key'] || config.headers['X-Admin-Key']) {
    return config
  }

  const apiKey = getApiKey()
  const adminKey = getAdminKey()

  if (config.url?.startsWith('/api/admin') && adminKey) {
    config.headers['X-Admin-Key'] = adminKey
  } else if (apiKey) {
    config.headers['X-API-Key'] = apiKey
  } else if (adminKey) {
    config.headers['X-Admin-Key'] = adminKey
  }

  return config
}, error => {
  return Promise.reject(error)
})

const app = createApp(App)

for (const [key, component] of Object.entries(ElementPlusIconsVue)) {
  app.component(key, component)
}

app.use(ElementPlus)
app.mount('#app')

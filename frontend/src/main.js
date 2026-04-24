import { createApp } from 'vue'
import ElementPlus from 'element-plus'
import 'element-plus/dist/index.css'
import * as ElementPlusIconsVue from '@element-plus/icons-vue'
import App from './App.vue'
import axios from 'axios'
import { getApiKey, getAdminKey } from './config.js'

// 配置 axios 全局拦截器，自动添加认证头
axios.interceptors.request.use(config => {
  // 跳过验证密钥的请求（避免循环）
  if (config.url === '/api/verify-key' || config.url === '/api/health') {
    return config
  }
  
  // 如果请求头中已经有密钥，不覆盖
  if (config.headers['X-API-Key'] || config.headers['X-Admin-Key']) {
    return config
  }
  
  // 自动添加密钥（优先 API Key，其次 Admin Key）
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

<template>
  <!-- API Key 验证对话框 -->
  <el-dialog 
    v-model="showAuthDialog" 
    title="身份验证" 
    width="500px"
    :close-on-click-modal="false"
    :close-on-press-escape="false"
    :show-close="false"
  >
    <el-alert
      title="请输入 API Key 或 Admin Key 以继续使用"
      type="info"
      :closable="false"
      show-icon
      style="margin-bottom: 20px;"
    />
    
    <el-tabs v-model="authTab" type="card">
      <el-tab-pane label="API Key" name="api">
        <el-form :model="authForm" label-width="100px">
          <el-form-item label="API Key">
            <el-input
              v-model="authForm.apiKey"
              type="password"
              placeholder="请输入 API Key"
              show-password
              @keyup.enter="verifyAuth"
            >
              <template #prefix>
                <el-icon><Lock /></el-icon>
              </template>
            </el-input>
          </el-form-item>
        </el-form>
      </el-tab-pane>
      
      <el-tab-pane label="Admin Key" name="admin">
        <el-form :model="authForm" label-width="100px">
          <el-form-item label="Admin Key">
            <el-input
              v-model="authForm.adminKey"
              type="password"
              placeholder="请输入 Admin Key"
              show-password
              @keyup.enter="verifyAuth"
            >
              <template #prefix>
                <el-icon><Key /></el-icon>
              </template>
            </el-input>
          </el-form-item>
        </el-form>
      </el-tab-pane>
    </el-tabs>
    
    <template #footer>
      <el-button type="primary" @click="verifyAuth" :loading="verifying">
        验证并进入
      </el-button>
    </template>
  </el-dialog>

  <!-- 主界面 -->
  <div class="container" v-if="authenticated">
    <div class="header">
      <h1>文件快传</h1>
      <p>大文件广域网传输</p>
      <div class="user-info" v-if="userIsAdmin">
        <el-tag type="success">管理员模式</el-tag>
        <el-button size="small" @click="logout" style="margin-left: 10px;">退出</el-button>
      </div>
    </div>
    
    <el-card class="main-card">
      <el-tabs v-model="activeTab">
        <el-tab-pane label="发送文件" name="send">
          <Upload />
        </el-tab-pane>
        <el-tab-pane label="接收文件" name="receive">
          <div class="receive-box">
            <el-input
              v-model="code"
              placeholder="请输入取件码"
              class="code-input"
              size="large"
            >
              <template #prefix>
                <el-icon><Key /></el-icon>
              </template>
            </el-input>
            <el-button type="primary" size="large" @click="retrieveFile" :loading="loading">
              确定
            </el-button>
          </div>
          <div v-if="fileInfo" class="file-info">
            <h3>发现文件：</h3>
            <p>文件名：{{ fileInfo.filename }}</p>
            <el-button type="success" @click="startDownload">点击下载</el-button>
          </div>
        </el-tab-pane>

        <el-tab-pane label="传输记录" name="transfers">
          <div class="records-box">
            <div class="records-header">
              <h3>传输记录</h3>
              <el-button type="primary" @click="loadTransferRecords" :loading="loadingTransfers">
                <el-icon><Refresh /></el-icon>
                刷新
              </el-button>
            </div>

            <el-table :data="transferRecords" style="width: 100%" v-loading="loadingTransfers">
              <el-table-column prop="filename" label="文件名" min-width="220" show-overflow-tooltip />
              <el-table-column prop="code" label="分享码" width="130">
                <template #default="scope">
                  <span class="code-cell">{{ scope.row.code }}</span>
                </template>
              </el-table-column>
              <el-table-column prop="created_at" label="上传时间" width="180">
                <template #default="scope">
                  {{ formatDate(scope.row.created_at) }}
                </template>
              </el-table-column>
              <el-table-column prop="expires_at" label="过期时间" width="180">
                <template #default="scope">
                  {{ scope.row.expires_at ? formatDate(scope.row.expires_at) : '长期有效' }}
                </template>
              </el-table-column>
              <el-table-column label="下载次数" width="130">
                <template #default="scope">
                  {{ formatDownloads(scope.row) }}
                </template>
              </el-table-column>
              <el-table-column prop="status" label="状态" width="110">
                <template #default="scope">
                  <el-tag :type="transferStatusType(scope.row.status)">
                    {{ transferStatusText(scope.row.status) }}
                  </el-tag>
                </template>
              </el-table-column>
              <el-table-column label="操作" width="190" fixed="right">
                <template #default="scope">
                  <el-button size="small" @click="copyShareCode(scope.row.code)">
                    <el-icon><CopyDocument /></el-icon>
                    复制
                  </el-button>
                  <el-button size="small" type="success" @click="downloadTransfer(scope.row)">
                    <el-icon><Download /></el-icon>
                    下载
                  </el-button>
                </template>
              </el-table-column>
            </el-table>
          </div>
        </el-tab-pane>
        
        <!-- 仅管理员可见的 API 设置选项卡 -->
        <el-tab-pane label="API设置" name="settings" v-if="userIsAdmin">
          <div class="settings-box">
            <el-tabs v-model="settingsTab" type="card">
              <!-- 使用API Key -->
              <el-tab-pane label="使用API Key" name="use">
                <h3>API Key 配置</h3>
                <p class="settings-desc">请输入您的API Key以使用上传和下载功能</p>
                <el-input
                  v-model="apiKeyInput"
                  type="password"
                  placeholder="请输入API Key"
                  show-password
                  class="api-key-input"
                  size="large"
                >
                  <template #prefix>
                    <el-icon><Lock /></el-icon>
                  </template>
                </el-input>
                <div class="settings-actions">
                  <el-button type="primary" @click="saveApiKey" :loading="savingKey">
                    保存
                  </el-button>
                  <el-button @click="clearApiKey">清除</el-button>
                </div>
                <div v-if="currentApiKey" class="api-key-status">
                  <el-alert
                    title="已配置API Key"
                    type="success"
                    :closable="false"
                    show-icon
                  />
                </div>
              </el-tab-pane>
              
              <!-- 管理API Key -->
              <el-tab-pane label="管理API Key" name="manage">
                <div class="manage-box">
                  <div class="manage-header">
                    <h3>API Key 管理</h3>
                    <el-button type="primary" @click="showCreateDialog = true">
                      <el-icon><Plus /></el-icon>
                      生成新Key
                    </el-button>
                  </div>
                  
                  <el-table :data="apiKeysList" style="width: 100%" v-loading="loadingKeys">
                    <el-table-column prop="key_prefix" label="Key前缀" width="150" />
                    <el-table-column prop="name" label="名称" />
                    <el-table-column prop="description" label="描述" />
                    <el-table-column prop="is_active" label="状态" width="100">
                      <template #default="scope">
                        <el-tag :type="scope.row.is_active ? 'success' : 'danger'">
                          {{ scope.row.is_active ? '启用' : '禁用' }}
                        </el-tag>
                      </template>
                    </el-table-column>
                    <el-table-column prop="created_at" label="创建时间" width="180">
                      <template #default="scope">
                        {{ formatDate(scope.row.created_at) }}
                      </template>
                    </el-table-column>
                    <el-table-column prop="last_used_at" label="最后使用" width="180">
                      <template #default="scope">
                        {{ scope.row.last_used_at ? formatDate(scope.row.last_used_at) : '从未使用' }}
                      </template>
                    </el-table-column>
                    <el-table-column label="操作" width="200">
                      <template #default="scope">
                        <el-button 
                          size="small" 
                          :type="scope.row.is_active ? 'warning' : 'success'"
                          @click="toggleKeyStatus(scope.row)"
                        >
                          {{ scope.row.is_active ? '禁用' : '启用' }}
                        </el-button>
                        <el-button 
                          size="small" 
                          type="danger" 
                          @click="deleteKey(scope.row)"
                        >
                          删除
                        </el-button>
                      </template>
                    </el-table-column>
                  </el-table>
                </div>
              </el-tab-pane>
            </el-tabs>
          </div>
          
          <!-- 创建API Key对话框 -->
          <el-dialog v-model="showCreateDialog" title="生成新API Key" width="500px">
            <el-form :model="newKeyForm" label-width="100px">
              <el-form-item label="名称">
                <el-input v-model="newKeyForm.name" placeholder="例如: 第三方客户端" />
              </el-form-item>
              <el-form-item label="描述">
                <el-input 
                  v-model="newKeyForm.description" 
                  type="textarea" 
                  :rows="3"
                  placeholder="描述此API Key的用途"
                />
              </el-form-item>
              <el-form-item label="有效期">
                <el-input-number 
                  v-model="newKeyForm.expires_days" 
                  :min="0" 
                  :max="3650"
                  placeholder="0表示永不过期"
                />
                <span style="margin-left: 10px; color: #666;">天 (0=永不过期)</span>
              </el-form-item>
            </el-form>
            <template #footer>
              <el-button @click="showCreateDialog = false">取消</el-button>
              <el-button type="primary" @click="createNewKey" :loading="creatingKey">生成</el-button>
            </template>
          </el-dialog>
          
          <!-- 显示新生成的Key -->
          <el-dialog v-model="showKeyResult" title="API Key已生成" width="600px">
            <el-alert
              title="请妥善保管此API Key，它只会显示一次！"
              type="warning"
              :closable="false"
              show-icon
              style="margin-bottom: 20px;"
            />
            <el-input
              :value="newGeneratedKey"
              readonly
              type="textarea"
              :rows="3"
              class="key-display"
            />
            <div style="margin-top: 15px;">
              <el-button @click="copyKey" type="primary">复制Key</el-button>
              <el-button @click="saveGeneratedKey">保存到本地</el-button>
            </div>
            <template #footer>
              <el-button type="primary" @click="showKeyResult = false">已保存</el-button>
            </template>
          </el-dialog>
        </el-tab-pane>
      </el-tabs>
    </el-card>
  </div>
</template>

<script setup>
import { ref, onMounted, watch } from 'vue'
import Upload from './components/Upload.vue'
import axios from 'axios'
import { ElMessage, ElMessageBox } from 'element-plus'
import { getApiKey, setApiKey, getAdminKey, setAdminKey } from './config.js'
import { CopyDocument, Download, Key, Lock, Plus, Refresh } from '@element-plus/icons-vue'

// 认证相关
const showAuthDialog = ref(false)
const authenticated = ref(false)
const userIsAdmin = ref(false)
const authTab = ref('api')
const authForm = ref({
  apiKey: '',
  adminKey: ''
})
const verifying = ref(false)

// 主界面
const activeTab = ref('send')
const code = ref('')
const loading = ref(false)
const fileInfo = ref(null)
const apiKeyInput = ref('')
const savingKey = ref(false)
const currentApiKey = ref('')
const settingsTab = ref('use')
const apiKeysList = ref([])
const loadingKeys = ref(false)
const transferRecords = ref([])
const loadingTransfers = ref(false)
const showCreateDialog = ref(false)
const showKeyResult = ref(false)
const newGeneratedKey = ref('')
const creatingKey = ref(false)
const newKeyForm = ref({
  name: '',
  description: '',
  expires_days: 0
})

onMounted(async () => {
  // 检查是否已有保存的密钥
  const savedApiKey = getApiKey()
  const savedAdminKey = getAdminKey()
  
  if (savedApiKey || savedAdminKey) {
    // 验证保存的密钥
    await verifyStoredKeys(savedApiKey, savedAdminKey)
  } else {
    // 检查是否需要认证
    await checkAuthRequired()
  }
  
  // Load saved API key for settings
  if (savedApiKey) {
    currentApiKey.value = savedApiKey
    apiKeyInput.value = '•'.repeat(Math.min(savedApiKey.length, 20))
  }
})

// 检查是否需要认证
const checkAuthRequired = async () => {
  try {
    const res = await axios.post('/api/verify-key', {})
    if (res.data.no_auth_required) {
      // 不需要认证，直接进入
      authenticated.value = true
      userIsAdmin.value = false
    } else {
      // 需要认证
      showAuthDialog.value = true
    }
  } catch (err) {
    // 默认需要认证
    showAuthDialog.value = true
  }
}

// 验证保存的密钥
const verifyStoredKeys = async (apiKey, adminKey) => {
  try {
    const res = await axios.post('/api/verify-key', {
      api_key: apiKey,
      admin_key: adminKey
    })
    
    if (res.data.valid) {
      authenticated.value = true
      userIsAdmin.value = res.data.is_admin || false
      
      if (res.data.is_admin) {
        ElMessage.success('欢迎，管理员！')
      }
    } else {
      // 密钥无效，清除并要求重新输入
      setApiKey('')
      setAdminKey('')
      showAuthDialog.value = true
      ElMessage.warning('保存的密钥已失效，请重新输入')
    }
  } catch (err) {
    showAuthDialog.value = true
  }
}

// 验证用户输入的密钥
const verifyAuth = async () => {
  const apiKey = authTab.value === 'api' ? authForm.value.apiKey : ''
  const adminKey = authTab.value === 'admin' ? authForm.value.adminKey : ''
  
  if (!apiKey && !adminKey) {
    ElMessage.warning('请输入密钥')
    return
  }
  
  verifying.value = true
  try {
    const res = await axios.post('/api/verify-key', {
      api_key: apiKey,
      admin_key: adminKey
    })
    
    if (res.data.valid) {
      // 保存密钥
      if (apiKey) {
        setApiKey(apiKey)
        currentApiKey.value = apiKey
      }
      if (adminKey) {
        setAdminKey(adminKey)
      }
      
      authenticated.value = true
      userIsAdmin.value = res.data.is_admin || false
      showAuthDialog.value = false
      
      if (res.data.is_admin) {
        ElMessage.success('欢迎，管理员！')
      } else {
        ElMessage.success('验证成功！')
      }
    } else {
      ElMessage.error('密钥无效，请检查后重试')
    }
  } catch (err) {
    ElMessage.error('验证失败: ' + (err.response?.data?.error || err.message))
  } finally {
    verifying.value = false
  }
}

// 退出登录
const logout = () => {
  setApiKey('')
  setAdminKey('')
  authenticated.value = false
  userIsAdmin.value = false
  authForm.value = { apiKey: '', adminKey: '' }
  showAuthDialog.value = true
  ElMessage.info('已退出')
}

const saveApiKey = () => {
  if (!apiKeyInput.value || apiKeyInput.value.startsWith('•')) {
    ElMessage.warning('请输入有效的API Key')
    return
  }
  savingKey.value = true
  setApiKey(apiKeyInput.value)
  currentApiKey.value = apiKeyInput.value
  ElMessage.success('API Key已保存')
  savingKey.value = false
}

const clearApiKey = () => {
  setApiKey('')
  currentApiKey.value = ''
  apiKeyInput.value = ''
  ElMessage.info('API Key已清除')
}

// API Key Management Functions
const loadAPIKeys = async () => {
  loadingKeys.value = true
  try {
    // axios 拦截器会自动添加 Admin Key
    const res = await axios.get('/api/admin/keys')
    apiKeysList.value = res.data.keys || []
  } catch (err) {
    if (err.response?.status === 401) {
      ElMessage.error('管理员权限验证失败')
    } else if (err.response?.status === 403) {
      ElMessage.warning('管理功能仅限本地网络访问，或需要设置ADMIN_KEY环境变量')
    } else {
      ElMessage.error('加载API Key列表失败')
    }
  } finally {
    loadingKeys.value = false
  }
}

const loadTransferRecords = async () => {
  loadingTransfers.value = true
  try {
    const endpoint = userIsAdmin.value ? '/api/admin/files' : '/api/files'
    const res = await axios.get(endpoint)
    transferRecords.value = res.data.files || []
  } catch (err) {
    if (err.response?.status === 401) {
      ElMessage.error('权限验证失败')
    } else if (err.response?.status === 403) {
      ElMessage.warning('当前密钥无法查看传输记录')
    } else {
      ElMessage.error('加载传输记录失败')
    }
  } finally {
    loadingTransfers.value = false
  }
}

const formatDownloads = (record) => {
  if (!record) return '0'
  if (!record.max_downloads) {
    return `${record.downloads || 0} / 不限`
  }
  return `${record.downloads || 0} / ${record.max_downloads}`
}

const transferStatusText = (status) => {
  if (status === 'expired') return '已过期'
  if (status === 'download_limit') return '已达上限'
  return '有效'
}

const transferStatusType = (status) => {
  if (status === 'expired') return 'info'
  if (status === 'download_limit') return 'danger'
  return 'success'
}

const copyShareCode = (shareCode) => {
  navigator.clipboard.writeText(shareCode).then(() => {
    ElMessage.success('分享码已复制')
  }).catch(() => {
    ElMessage.error('复制失败')
  })
}

const createNewKey = async () => {
  if (!newKeyForm.value.name) {
    ElMessage.warning('请输入名称')
    return
  }
  
  creatingKey.value = true
  try {
    // axios 拦截器会自动添加 Admin Key
    const res = await axios.post('/api/admin/keys', newKeyForm.value)
    newGeneratedKey.value = res.data.key
    showCreateDialog.value = false
    showKeyResult.value = true
    // Reset form
    newKeyForm.value = { name: '', description: '', expires_days: 0 }
    // Reload list
    await loadAPIKeys()
  } catch (err) {
    if (err.response?.status === 401) {
      ElMessage.error('管理员权限验证失败')
    } else if (err.response?.status === 403) {
      ElMessage.warning('管理功能仅限本地网络访问，或需要设置ADMIN_KEY环境变量')
    } else {
      ElMessage.error('生成API Key失败: ' + (err.response?.data?.error || err.message))
    }
  } finally {
    creatingKey.value = false
  }
}

const toggleKeyStatus = async (key) => {
  try {
    // axios 拦截器会自动添加 Admin Key
    await axios.patch(`/api/admin/keys/${key.id}`, {
      is_active: !key.is_active
    })
    ElMessage.success('状态已更新')
    await loadAPIKeys()
  } catch (err) {
    ElMessage.error('更新失败')
  }
}

const deleteKey = async (key) => {
  try {
    await ElMessageBox.confirm(
      `确定要删除API Key "${key.name || key.key_prefix}" 吗？此操作不可恢复。`,
      '确认删除',
      {
        confirmButtonText: '删除',
        cancelButtonText: '取消',
        type: 'warning',
      }
    )
    
    // axios 拦截器会自动添加 Admin Key
    await axios.delete(`/api/admin/keys/${key.id}`)
    ElMessage.success('已删除')
    await loadAPIKeys()
  } catch (err) {
    if (err === 'cancel') {
      return
    }
    ElMessage.error('删除失败')
  }
}

const copyKey = () => {
  navigator.clipboard.writeText(newGeneratedKey.value).then(() => {
    ElMessage.success('已复制到剪贴板')
  }).catch(() => {
    ElMessage.error('复制失败')
  })
}

const saveGeneratedKey = () => {
  setApiKey(newGeneratedKey.value)
  currentApiKey.value = newGeneratedKey.value
  ElMessage.success('已保存到本地')
}

const formatDate = (dateStr) => {
  if (!dateStr) return ''
  const date = new Date(dateStr)
  return date.toLocaleString('zh-CN')
}

// Watch settings tab change
watch(settingsTab, (newTab) => {
  if (newTab === 'manage') {
    loadAPIKeys()
  }
})

watch(activeTab, (newTab) => {
  if (newTab === 'transfers') {
    loadTransferRecords()
  }
})

const retrieveFile = async () => {
  if (!code.value) return
  loading.value = true
  try {
    // axios 拦截器会自动添加密钥，无需手动处理
    const res = await axios.get(`/api/retrieve/${code.value}`)
    fileInfo.value = res.data
  } catch (err) {
    ElMessage.error('无效的取件码或文件未找到')
    fileInfo.value = null
  } finally {
    loading.value = false
  }
}

const withAuthQuery = (url) => {
  const apiKey = getApiKey()
  const adminKey = getAdminKey()

  if (userIsAdmin.value && adminKey) {
    const separator = url.includes('?') ? '&' : '?'
    return `${url}${separator}admin_key=${encodeURIComponent(adminKey)}`
  }

  if (apiKey) {
    const separator = url.includes('?') ? '&' : '?'
    return `${url}${separator}api_key=${encodeURIComponent(apiKey)}`
  }

  if (adminKey) {
    const separator = url.includes('?') ? '&' : '?'
    return `${url}${separator}admin_key=${encodeURIComponent(adminKey)}`
  }

  return url
}

const triggerDownload = (url) => {
  const link = document.createElement('a')
  link.href = url
  link.target = '_blank'
  document.body.appendChild(link)
  link.click()
  document.body.removeChild(link)
}

const downloadTransfer = (record) => {
  if (!record?.code) return
  triggerDownload(withAuthQuery(`/files/${record.code}`))
}

const startDownload = () => {
  if (fileInfo.value) {
    triggerDownload(withAuthQuery(fileInfo.value.url))
  }
}
</script>

<style>
body {
  background-color: #f0f2f5;
  margin: 0;
  font-family: 'Helvetica Neue', Helvetica, 'PingFang SC', 'Hiragino Sans GB', 'Microsoft YaHei', '微软雅黑', Arial, sans-serif;
}
.container {
  max-width: 1100px;
  margin: 50px auto;
  padding: 20px;
}
.header {
  text-align: center;
  margin-bottom: 30px;
  position: relative;
}
.user-info {
  position: absolute;
  top: 0;
  right: 0;
}
.main-card {
  min-height: 400px;
}
.receive-box {
  display: flex;
  gap: 10px;
  margin-top: 50px;
  justify-content: center;
}
.code-input {
  width: 350px;
}
.file-info {
  margin-top: 30px;
  text-align: center;
  background: #f9f9f9;
  padding: 20px;
  border-radius: 8px;
}
.settings-box {
  padding: 20px;
  max-width: 500px;
  margin: 0 auto;
}
.settings-desc {
  color: #666;
  margin-bottom: 20px;
  font-size: 14px;
}
.api-key-input {
  margin-bottom: 20px;
}
.settings-actions {
  display: flex;
  gap: 10px;
  margin-bottom: 20px;
}
.api-key-status {
  margin-top: 20px;
}
.manage-box {
  padding: 20px 0;
}
.manage-header {
  display: flex;
  justify-content: space-between;
  align-items: center;
  margin-bottom: 20px;
}
.records-box {
  padding: 20px 0;
}
.records-header {
  display: flex;
  justify-content: space-between;
  align-items: center;
  margin-bottom: 20px;
}
.code-cell {
  font-family: monospace;
  font-weight: 600;
}
.key-display {
  font-family: monospace;
  font-size: 14px;
}
</style>

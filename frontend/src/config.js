// API Configuration
export const API_KEY = import.meta.env.VITE_API_KEY || ''

// API Base URL
export const API_BASE_URL = import.meta.env.VITE_API_BASE_URL || ''

// Get API key from localStorage or environment
export function getApiKey() {
  return localStorage.getItem('api_key') || API_KEY
}

// Set API key to localStorage
export function setApiKey(key) {
  if (key) {
    localStorage.setItem('api_key', key)
  } else {
    localStorage.removeItem('api_key')
  }
}

// Get admin key from localStorage
export function getAdminKey() {
  return localStorage.getItem('admin_key') || ''
}

// Set admin key to localStorage
export function setAdminKey(key) {
  if (key) {
    localStorage.setItem('admin_key', key)
  } else {
    localStorage.removeItem('admin_key')
  }
}

// Check if user is admin
export function isAdmin() {
  return !!getAdminKey()
}

// Get headers with API key
export function getApiHeaders() {
  const key = getApiKey()
  const headers = {}
  if (key) {
    headers['X-API-Key'] = key
  }
  return headers
}

// Get headers with admin key
export function getAdminHeaders() {
  const key = getAdminKey()
  const headers = {}
  if (key) {
    headers['X-Admin-Key'] = key
  }
  return headers
}

import {createApp} from 'vue'
import App from './App.vue'
import './style.css';
import { installUIErrorCapture } from './uiErrors';

const app = createApp(App);
installUIErrorCapture(app);
app.mount('#app')

import { Routes } from '@angular/router';

export const routes: Routes = [
  { path: '', pathMatch: 'full', redirectTo: 'dashboard' },
  { path: 'dashboard', title: 'Dashboard · DAS', loadComponent: () => import('./features/dashboard/dashboard').then((m) => m.Dashboard) },
  { path: 'spectrum', title: 'Spectrum analyzer · DAS', loadComponent: () => import('./features/spectrum/spectrum').then((m) => m.SpectrumPage) },
  { path: 'nodes/:id', title: 'Remote Node · DAS', loadComponent: () => import('./features/node-detail/node-detail').then((m) => m.NodeDetail) },
  { path: '**', redirectTo: 'dashboard' }
];

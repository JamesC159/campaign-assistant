import { createBrowserRouter, type RouteObject } from 'react-router'
import { Layout } from './components/Layout'
import { Home } from './routes/Home'
import { NotFound } from './routes/NotFound'
import { RouteError } from './routes/RouteError'

export const routes: RouteObject[] = [
  {
    path: '/',
    element: <Layout />,
    errorElement: <RouteError />,
    children: [
      { index: true, element: <Home /> },
      { path: '*', element: <NotFound /> },
    ],
  },
]

export const router = createBrowserRouter(routes)

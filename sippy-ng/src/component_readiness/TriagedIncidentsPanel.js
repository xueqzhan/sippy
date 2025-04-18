import { Grid } from '@mui/material'
import EventEmitter from 'eventemitter3'
import PropTypes from 'prop-types'
import React, { Fragment } from 'react'
import TriagedIncidentGroups from './TriagedIncidentGroups'
import TriagedTestDetails from './TriagedTestDetails'
import TriagedVariants from './TriagedVariants'

export default function TriagedIncidentsPanel(props) {
  const eventEmitter = new EventEmitter()

  return (
    <Fragment>
      <Grid>
        <TriagedIncidentGroups
          eventEmitter={eventEmitter}
          triagedIncidents={props.triagedIncidents}
        />
        <TriagedVariants eventEmitter={eventEmitter} />
        <TriagedTestDetails eventEmitter={eventEmitter} />
      </Grid>
    </Fragment>
  )
}

TriagedIncidentsPanel.propTypes = {
  triagedIncidents: PropTypes.array,
}
